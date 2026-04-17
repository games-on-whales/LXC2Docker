package api

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/lxc"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
	"golang.org/x/sys/unix"
)

func (h *Handler) ping(w http.ResponseWriter, r *http.Request) {
	// Portainer (and docker CLI) feature-detect from /_ping headers — the
	// body is just "OK". Surface the same header set Docker does so the
	// client doesn't have to guess swarm/builder/experimental state.
	hdr := w.Header()
	hdr.Set("API-Version", apiVersion)
	hdr.Set("Docker-Experimental", "false")
	hdr.Set("OSType", "linux")
	hdr.Set("Builder-Version", "1")
	hdr.Set("Swarm", "inactive")
	hdr.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	hdr.Set("Pragma", "no-cache")
	hdr.Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	// HEAD requests (used by Docker's health probe and some haproxy
	// setups) must not include a body; ResponseWriter would buffer it
	// anyway, but being explicit keeps the content-length zero.
	if r.Method != http.MethodHead {
		w.Write([]byte("OK"))
	}
}

func (h *Handler) version(w http.ResponseWriter, r *http.Request) {
	var uname unix.Utsname
	unix.Uname(&uname)

	resp := VersionResponse{
		Version:       "24.0.0",
		APIVersion:    apiVersion,
		MinAPIVersion: "1.12",
		GitCommit:     "lxc",
		GoVersion:     runtime.Version(),
		Os:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		KernelVersion: unameRelease(uname),
		BuildTime:     "N/A",
		Platform:      VersionPlatform{Name: "docker-lxc-daemon"},
		Components: []VersionComponent{
			{
				Name:    "Engine",
				Version: "24.0.0",
				Details: map[string]string{
					"ApiVersion":    apiVersion,
					"Arch":          runtime.GOARCH,
					"GitCommit":     "lxc",
					"GoVersion":     runtime.Version(),
					"KernelVersion": unameRelease(uname),
					"MinAPIVersion": "1.12",
					"Os":            runtime.GOOS,
				},
			},
			{Name: "docker-lxc-daemon", Version: "pr10"},
		},
	}
	jsonResponse(w, http.StatusOK, resp)
}

func (h *Handler) info(w http.ResponseWriter, r *http.Request) {
	containers := h.store.ListContainers()
	images := h.store.ListImages()

	running := 0
	for _, c := range containers {
		state, _ := h.mgr.State(c.ID)
		if state == "running" {
			running++
		}
	}

	var si unix.Sysinfo_t
	unix.Sysinfo(&si)

	var uname unix.Utsname
	unix.Uname(&uname)

	warnings := []string{}
	if h.mgr.UsePVE() {
		// No-op today; reserved for surfacing PVE-specific warnings later.
	}

	resp := InfoResponse{
		ID:                 "docker-lxc-daemon",
		Name:               hostname(),
		Containers:         len(containers),
		ContainersRunning:  running,
		ContainersPaused:   0,
		ContainersStopped:  len(containers) - running,
		Images:             len(images),
		Driver:             "lxc",
		MemoryLimit:        true,
		SwapLimit:          true,
		KernelVersion:      unameRelease(uname),
		OperatingSystem:    osPrettyName(),
		OSVersion:          unameRelease(uname),
		OSType:             "linux",
		Architecture:       runtime.GOARCH,
		NCPU:               runtime.NumCPU(),
		MemTotal:           int64(si.Totalram) * int64(si.Unit),
		DockerRootDir:      h.mgr.LXCPath(),
		ServerVersion:      "24.0.0",
		CgroupDriver:       "systemd",
		CgroupVersion:      cgroupVersion(),
		DefaultRuntime:     "lxc",
		Runtimes:           map[string]any{"lxc": map[string]string{"path": "lxc-start"}},
		Plugins:            InfoPlugins{Volume: []string{"local"}, Network: []string{"bridge", "host"}},
		Labels:             []string{},
		ExperimentalBuild:  false,
		SystemTime:         time.Now().UTC().Format(time.RFC3339Nano),
		LiveRestoreEnabled: true,
		IndexServerAddress: "https://index.docker.io/v1/",
		RegistryConfig:     map[string]any{"IndexConfigs": map[string]any{}, "InsecureRegistryCIDRs": []string{}},
		Warnings:           warnings,
		SecurityOptions:    []string{"name=no-new-privileges"},
		ContainerdCommit:   VersionComponent{Name: "not-applicable"},
		RuncCommit:         VersionComponent{Name: "not-applicable"},
		InitCommit:         VersionComponent{Name: "not-applicable"},
	}
	jsonResponse(w, http.StatusOK, resp)
}

// osPrettyName returns /etc/os-release PRETTY_NAME (e.g. "Debian GNU/Linux
// 12 (bookworm)") so Portainer's dashboard shows a human-readable OS label.
func osPrettyName() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "PRETTY_NAME=") {
			continue
		}
		v := strings.TrimPrefix(line, "PRETTY_NAME=")
		v = strings.Trim(v, `"`)
		if v != "" {
			return v
		}
	}
	return "Linux"
}

// cgroupVersion returns "2" on a unified cgroup v2 host (/sys/fs/cgroup is
// cgroup2fs) and "1" otherwise, matching the format Docker's /info uses.
func cgroupVersion() string {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return "2"
	}
	return "1"
}

// --- network stubs (Docker clients query networks when creating containers) ---
func (h *Handler) listNetworks(w http.ResponseWriter, r *http.Request) {
	filters, err := parseListFilters(r.URL.Query().Get("filters"))
	if err != nil {
		errResponse(w, http.StatusBadRequest, "invalid filters: "+err.Error())
		return
	}
	networks := []NetworkResource{}
	// The built-in gow network synthesises a store-less record; build a
	// shim so the same filter matcher decides whether to include it.
	gowShim := &store.NetworkRecord{
		ID:     "gow",
		Name:   "gow",
		Driver: "bridge",
		Scope:  "local",
		Labels: map[string]string{},
	}
	if matchesNetworkFilters(gowShim, filters) {
		networks = append(networks, h.defaultNetworkResource())
	}
	for _, n := range h.store.ListNetworks() {
		if !matchesNetworkFilters(n, filters) {
			continue
		}
		networks = append(networks, h.networkResource(n))
	}
	jsonResponse(w, http.StatusOK, networks)
}

func (h *Handler) inspectNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "gow" {
		jsonResponse(w, http.StatusOK, h.defaultNetworkResource())
		return
	}
	if n := h.store.GetNetwork(id); n != nil {
		res := h.networkResource(n)
		res.Containers = h.networkContainersFor(n.Name, n.ID)
		jsonResponse(w, http.StatusOK, res)
		return
	}
	errResponse(w, http.StatusNotFound, "network not found")
}

func (h *Handler) createNetwork(w http.ResponseWriter, r *http.Request) {
	var req NetworkCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" {
		errResponse(w, http.StatusBadRequest, "network name is required")
		return
	}
	if existing := h.store.GetNetwork(req.Name); existing != nil {
		jsonResponse(w, http.StatusCreated, NetworkCreateResponse{ID: existing.ID})
		return
	}
	id := generateID()[:12]
	rec := &store.NetworkRecord{
		ID:         id,
		Name:       req.Name,
		Driver:     orDefault(req.Driver, "bridge"),
		Scope:      "local",
		CreatedAt:  time.Now().UTC(),
		Labels:     req.Labels,
		Options:    req.Options,
		Internal:   req.Internal,
		Attachable: req.Attachable,
		IPAM:       ipamFromRequest(req.IPAM),
	}
	if err := h.store.AddNetwork(rec); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.publishEvent("network", "create", rec.ID, map[string]string{"name": rec.Name, "type": rec.Driver})
	jsonResponse(w, http.StatusCreated, NetworkCreateResponse{ID: rec.ID})
}

func (h *Handler) removeNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "gow" {
		errResponse(w, http.StatusForbidden, "default network cannot be removed")
		return
	}
	n := h.store.GetNetwork(id)
	if n == nil {
		errResponse(w, http.StatusNotFound, "network not found")
		return
	}
	if containers := h.networkContainersFor(n.Name, n.ID); len(containers) > 0 {
		errResponse(w, http.StatusConflict, "network is in use")
		return
	}
	if err := h.store.RemoveNetwork(n.ID); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.publishEvent("network", "destroy", n.ID, map[string]string{"name": n.Name, "type": n.Driver})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) connectNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	network := h.store.GetNetwork(id)
	if id != "gow" && network == nil {
		errResponse(w, http.StatusNotFound, "network not found")
		return
	}
	var req NetworkConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	containerID := h.resolveID(req.Container)
	if containerID == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	rec := h.store.GetContainer(containerID)
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	if rec.Networks == nil {
		rec.Networks = defaultContainerNetworks(rec)
	}
	networkName := id
	networkID := id
	if network != nil {
		networkName = network.Name
		networkID = network.ID
	}
	rec.Networks[networkName] = store.NetworkAttachment{
		NetworkID:  networkID,
		IPAddress:  orDefault(req.EndpointConfig.IPAddress, rec.IPAddress),
		Gateway:    orDefault(req.EndpointConfig.Gateway, lxc.BridgeGW),
		MacAddress: req.EndpointConfig.MacAddress,
		EndpointID: endpointID(containerID, networkName),
		Aliases:    append([]string{}, req.EndpointConfig.Aliases...),
		Links:      append([]string{}, req.EndpointConfig.Links...),
		DriverOpts: copyStringMap(req.EndpointConfig.DriverOpts),
	}
	if err := h.store.AddContainer(rec); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.publishEvent("network", "connect", networkID, map[string]string{
		"name":      networkName,
		"container": rec.Name,
	})
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) disconnectNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	network := h.store.GetNetwork(id)
	if id != "gow" && network == nil {
		errResponse(w, http.StatusNotFound, "network not found")
		return
	}
	var req NetworkConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	containerID := h.resolveID(req.Container)
	if containerID == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	rec := h.store.GetContainer(containerID)
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	networkName := id
	networkID := id
	if network != nil {
		networkName = network.Name
		networkID = network.ID
	}
	if networkName == "gow" {
		errResponse(w, http.StatusForbidden, "default network cannot be disconnected")
		return
	}
	delete(rec.Networks, networkName)
	if err := h.store.AddContainer(rec); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.publishEvent("network", "disconnect", networkID, map[string]string{
		"name":      networkName,
		"container": rec.Name,
	})
	w.WriteHeader(http.StatusOK)
}

// events implements GET /events and streams daemon lifecycle events using the
// Docker-compatible JSON event stream shape. Honors the since/until
// timestamps Docker clients use to replay events around a reconnect.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	since, err := parseLogTimestamp(r.URL.Query().Get("since"), "since")
	if err != nil {
		errResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	until, err := parseLogTimestamp(r.URL.Query().Get("until"), "until")
	if err != nil {
		errResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	ch := h.eventsHub.subscribe()
	defer h.eventsHub.unsubscribe(ch)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	// Non-streaming mode: when until is set and already in the past, just
	// return an empty body. Docker clients use this form to probe whether
	// the endpoint is alive.
	if until != nil && !until.After(time.Now()) {
		return
	}
	// When until is in the future, close the stream at that deadline.
	var deadline <-chan time.Time
	if until != nil {
		deadline = time.After(time.Until(*until))
	}
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			return
		case evt := <-ch:
			if since != nil && evt.TimeNano < since.UnixNano() {
				continue
			}
			if until != nil && evt.TimeNano > until.UnixNano() {
				return
			}
			if !eventMatches(r, evt) {
				continue
			}
			if err := enc.Encode(evt); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (h *Handler) systemDiskUsage(w http.ResponseWriter, r *http.Request) {
	images := h.store.ListImages()
	containers := h.store.ListContainers()
	volumes := h.store.ListVolumes()

	out := SystemDiskUsage{
		Images:     make([]ImageUsage, 0, len(images)),
		Containers: make([]ContainerUsage, 0, len(containers)),
		Volumes:    make([]VolumeUsage, 0, len(volumes)),
		BuildCache: []any{},
	}

	// Pre-compute how many containers reference each image (by normalised
	// ref) so the /system/df Images rows can report the count Portainer's
	// resource overview reads.
	containersPerImage := map[string]int{}
	for _, c := range containers {
		containersPerImage[normalizeImageRef(c.Image)]++
	}
	for _, img := range images {
		repo, tag := splitImageRef(img.Ref)
		size := h.imageSize(img)
		out.Images = append(out.Images, ImageUsage{
			ID:         "sha256:" + img.ID,
			Repository: repo,
			Tag:        tag,
			Size:       size,
			Containers: containersPerImage[normalizeImageRef(img.Ref)],
			CreatedAt:  img.Created.Format(time.RFC3339),
			RepoTags:   []string{img.Ref},
		})
		out.LayersSize += size
	}
	for _, c := range containers {
		state, _ := h.mgr.State(c.ID)
		sizeRootfs, _ := dirSize(h.mgr.RootfsPath(c.ID))
		out.Containers = append(out.Containers, ContainerUsage{
			ID:         c.ID,
			Names:      []string{"/" + c.Name},
			Image:      normalizeImageRef(c.Image),
			ImageID:    c.ImageID,
			Command:    strings.Join(append(c.Entrypoint, c.Cmd...), " "),
			Created:    c.Created.Unix(),
			State:      state,
			Status:     stateToStatus(state, c.Created),
			SizeRootFs: sizeRootfs,
		})
	}
	for _, v := range volumes {
		size, _ := dirSize(v.Mountpoint)
		vu := volumeUsage(h.store, v, size)
		out.Volumes = append(out.Volumes, vu)
	}
	jsonResponse(w, http.StatusOK, out)
}

func (h *Handler) defaultNetworkResource() NetworkResource {
	return NetworkResource{
		Name:       "gow",
		ID:         "gow",
		Created:    time.Unix(0, 0).UTC().Format(time.RFC3339),
		Scope:      "local",
		Driver:     "bridge",
		EnableIPv4: true,
		EnableIPv6: false,
		IPAM: map[string]any{
			"Driver": "default",
			"Config": []map[string]string{{
				"Subnet":  "10.100.0.0/24",
				"Gateway": lxc.BridgeGW,
			}},
		},
		Options:    map[string]string{},
		Labels:     map[string]string{},
		Containers: h.networkContainers(),
	}
}

func (h *Handler) networkResource(n *store.NetworkRecord) NetworkResource {
	return NetworkResource{
		Name:       n.Name,
		ID:         n.ID,
		Created:    n.CreatedAt.Format(time.RFC3339),
		Scope:      orDefault(n.Scope, "local"),
		Driver:     orDefault(n.Driver, "bridge"),
		EnableIPv4: true,
		EnableIPv6: false,
		Internal:   n.Internal,
		Attachable: n.Attachable,
		IPAM:       ipamToResource(n.IPAM),
		Options:    n.Options,
		Labels:     n.Labels,
		Containers: h.networkContainersFor(n.Name, n.ID),
	}
}

// ipamFromRequest copies the create-request IPAM block into the store's
// shape. Returns nil when the caller didn't submit an IPAM block so older
// persisted records stay untouched.
func ipamFromRequest(in *IPAMRequest) *store.NetworkIPAM {
	if in == nil {
		return nil
	}
	out := &store.NetworkIPAM{
		Driver:  in.Driver,
		Options: in.Options,
	}
	for _, c := range in.Config {
		out.Config = append(out.Config, store.NetworkIPAMConfig{
			Subnet:     c.Subnet,
			IPRange:    c.IPRange,
			Gateway:    c.Gateway,
			AuxAddress: c.AuxAddress,
		})
	}
	return out
}

// ipamToResource materialises the shape Docker's inspect response uses from
// a stored IPAM block. Falls back to "default" driver with an empty Config
// list so network detail views always see a populated block.
func ipamToResource(in *store.NetworkIPAM) map[string]any {
	if in == nil {
		return map[string]any{
			"Driver": "default",
			"Config": []map[string]string{},
		}
	}
	cfg := make([]map[string]string, 0, len(in.Config))
	for _, c := range in.Config {
		entry := map[string]string{}
		if c.Subnet != "" {
			entry["Subnet"] = c.Subnet
		}
		if c.IPRange != "" {
			entry["IPRange"] = c.IPRange
		}
		if c.Gateway != "" {
			entry["Gateway"] = c.Gateway
		}
		cfg = append(cfg, entry)
	}
	return map[string]any{
		"Driver":  orDefault(in.Driver, "default"),
		"Config":  cfg,
		"Options": in.Options,
	}
}

func (h *Handler) networkContainers() map[string]NetworkEndpoint {
	return h.networkContainersFor("gow", "gow")
}

func (h *Handler) networkContainersFor(networkName, networkID string) map[string]NetworkEndpoint {
	out := map[string]NetworkEndpoint{}
	for _, c := range h.store.ListContainers() {
		if len(c.Networks) == 0 {
			c.Networks = defaultContainerNetworks(c)
		}
		attachment, ok := c.Networks[networkName]
		if !ok {
			continue
		}
		if attachment.IPAddress == "" {
			continue
		}
		out[c.ID] = NetworkEndpoint{
			Name:        c.Name,
			EndpointID:  orDefault(attachment.EndpointID, endpointID(c.ID, networkName)),
			MacAddress:  attachment.MacAddress,
			IPv4Address: attachment.IPAddress + "/24",
		}
	}
	return out
}

func (h *Handler) networkExists(id string) bool {
	return id == "gow" || h.store.GetNetwork(id) != nil
}

func eventMatches(r *http.Request, evt EventMessage) bool {
	filters := r.URL.Query().Get("filters")
	if filters == "" {
		return true
	}
	var decoded map[string]map[string]bool
	if err := json.Unmarshal([]byte(filters), &decoded); err != nil {
		return true
	}
	if types, ok := decoded["type"]; ok && len(types) > 0 && !types[evt.Type] {
		return false
	}
	if events, ok := decoded["event"]; ok && len(events) > 0 && !events[evt.Action] {
		return false
	}
	// container/image/volume/network filters match the actor's id or name
	// AND constrain the event type to match. Docker drops a volume event
	// when the subscriber requested `container=foo`; match that so
	// Portainer's per-container subscriptions don't see unrelated noise.
	for _, key := range []string{"container", "image", "volume", "network"} {
		vals, ok := decoded[key]
		if !ok || len(vals) == 0 {
			continue
		}
		if key != evt.Type {
			return false
		}
		name := evt.Actor.Attributes["name"]
		if !vals[evt.Actor.ID] && !vals[name] {
			return false
		}
	}
	// label filter: require every `key=value` (or bare `key`) to be present
	// in the actor attributes.
	if labels, ok := decoded["label"]; ok && len(labels) > 0 {
		for wanted := range labels {
			k, v, hasValue := strings.Cut(wanted, "=")
			got, present := evt.Actor.Attributes[k]
			if !present {
				return false
			}
			if hasValue && got != v {
				return false
			}
		}
	}
	return true
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func splitImageRef(ref string) (string, string) {
	if before, after, ok := strings.Cut(ref, ":"); ok {
		return before, after
	}
	return ref, "latest"
}

// --- helpers ---

const apiVersion = "1.43"

func jsonResponse(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func errResponse(w http.ResponseWriter, code int, msg string) {
	jsonResponse(w, code, ErrorResponse{Message: msg})
}

func unameRelease(u unix.Utsname) string {
	b := make([]byte, 0, len(u.Release))
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// statusRecorder wraps http.ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
