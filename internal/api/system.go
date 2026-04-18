package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
	"github.com/gorilla/mux"
	"golang.org/x/sys/unix"
)

// execCommand is a thin wrapper around exec.Command+Output so tests (and the
// lxc-start --version lookup) have a single seam. It discards stderr on
// error since we only care about successful runs.
func execCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// ping writes the headers Docker clients (including Portainer) expect when
// probing a daemon. The empty "OK" body is legal; the headers are the useful
// part — Portainer keys its health check off API-Version and OSType.
func (h *Handler) ping(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("API-Version", apiVersion)
	w.Header().Set("OSType", "linux")
	w.Header().Set("Ostype", "linux")
	w.Header().Set("Docker-Experimental", "false")
	w.Header().Set("Builder-Version", "1")
	w.Header().Set("Swarm", "inactive")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write([]byte("OK"))
	}
}

func (h *Handler) version(w http.ResponseWriter, r *http.Request) {
	var uname unix.Utsname
	unix.Uname(&uname)

	engineVersion := "24.0.0-lxc"
	components := []VersionComponent{
		{
			Name:    "Engine",
			Version: engineVersion,
			Details: map[string]string{
				"ApiVersion":    apiVersion,
				"Arch":          runtime.GOARCH,
				"BuildTime":     "N/A",
				"Experimental":  "false",
				"GitCommit":     "lxc",
				"GoVersion":     runtime.Version(),
				"KernelVersion": unameRelease(uname),
				"MinAPIVersion": "1.12",
				"Os":            runtime.GOOS,
			},
		},
	}
	if v := lxcToolVersion(); v != "" {
		components = append(components, VersionComponent{
			Name:    "LXC",
			Version: v,
			Details: map[string]string{},
		})
	}

	resp := VersionResponse{
		Platform:      map[string]string{"Name": "docker-lxc-daemon"},
		Components:    components,
		Version:       engineVersion,
		APIVersion:    apiVersion,
		MinAPIVersion: "1.12",
		GitCommit:     "lxc",
		GoVersion:     runtime.Version(),
		Os:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		KernelVersion: unameRelease(uname),
		BuildTime:     "N/A",
	}
	jsonResponse(w, http.StatusOK, resp)
}

// lxcToolVersion runs `lxc-start --version` and returns the first line. We
// only need this for /version cosmetics, so a missing binary is silently
// ignored (the Engine component is still reported).
func lxcToolVersion() string {
	out, err := execCommand("lxc-start", "--version")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func (h *Handler) info(w http.ResponseWriter, r *http.Request) {
	containers := h.store.ListContainers()
	images := h.store.ListImages()

	running, paused := 0, 0
	for _, c := range containers {
		state, _ := h.mgr.State(c.ID)
		switch state {
		case "running":
			running++
		case "paused", "frozen":
			running++
			paused++
		}
	}

	var si unix.Sysinfo_t
	unix.Sysinfo(&si)

	var uname unix.Utsname
	unix.Uname(&uname)

	resp := InfoResponse{
		ID:                 "docker-lxc-daemon",
		Containers:         len(containers),
		ContainersRunning:  running,
		ContainersPaused:   paused,
		ContainersStopped:  len(containers) - running,
		Images:             len(images),
		Driver:             "lxc",
		MemoryLimit:        true,
		SwapLimit:          true,
		KernelVersion:      unameRelease(uname),
		OperatingSystem:    "docker-lxc-daemon",
		OSVersion:          unameRelease(uname),
		OSType:             "linux",
		Architecture:       unameArch(runtime.GOARCH),
		NCPU:               runtime.NumCPU(),
		MemTotal:           int64(si.Totalram) * int64(si.Unit),
		DockerRootDir:      h.mgr.LXCPath(),
		ServerVersion:      "24.0.0-lxc",
		Name:               hostname(),
		IndexServerAddress: "https://index.docker.io/v1/",
		RegistryConfig: RegistryConfig{
			AllowNondistributableArtifactsCIDRs:     []string{},
			AllowNondistributableArtifactsHostnames: []string{},
			InsecureRegistryCIDRs:                   []string{},
			IndexConfigs: map[string]any{
				"docker.io": map[string]any{
					"Name":     "docker.io",
					"Mirrors":  []string{},
					"Secure":   true,
					"Official": true,
				},
			},
			Mirrors: []string{},
		},
		Swarm: SwarmInfo{
			LocalNodeState: "inactive",
			RemoteManagers: []string{},
		},
		Plugins: PluginsInfo{
			Volume:        []string{"local"},
			Network:       []string{"bridge", "host", "none"},
			Authorization: []string{},
			Log:           []string{"json-file"},
		},
		DefaultRuntime:     "lxc",
		Runtimes:           map[string]any{"lxc": map[string]string{"path": "lxc-start"}},
		LiveRestoreEnabled: true,
		Isolation:          "default",
		CgroupDriver:       detectCgroupDriver(),
		CgroupVersion:      detectCgroupVersion(),
		SystemTime:         time.Now().UTC().Format(time.RFC3339Nano),
		Labels:             []string{},
		ExperimentalBuild:  false,
		HTTPProxy:          os.Getenv("HTTP_PROXY"),
		HTTPSProxy:         os.Getenv("HTTPS_PROXY"),
		NoProxy:            os.Getenv("NO_PROXY"),
		SecurityOptions:    []string{"name=seccomp,profile=default"},
		LoggingDriver:      "json-file",
		Warnings:           []string{},
		ClientInfo:         map[string]string{},
	}
	jsonResponse(w, http.StatusOK, resp)
}

func unameArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "arm":
		return "armv7l"
	case "386":
		return "i686"
	case "ppc64le":
		return "ppc64le"
	case "s390x":
		return "s390x"
	case "riscv64":
		return "riscv64"
	}
	return goarch
}

// detectCgroupVersion returns "1" or "2" based on whether unified cgroup v2 is
// mounted. Portainer displays this on its engine info page.
func detectCgroupVersion() string {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return "2"
	}
	return "1"
}

// detectCgroupDriver reports the cgroup driver. LXC uses cgroupfs by default.
func detectCgroupDriver() string {
	return "cgroupfs"
}

// --- networks ---
//
// The daemon runs a single managed bridge (gow0) plus the usual Docker meta
// networks (host/none). Portainer's Networks tab lists these; its container
// create form reads Driver/Scope/IPAM to populate fields. We return a
// realistic snapshot rather than an empty array so the UI doesn't show the
// engine as misconfigured.

type networkCreateRequest struct {
	Name           string            `json:"Name"`
	Driver         string            `json:"Driver"`
	CheckDuplicate bool              `json:"CheckDuplicate"`
	Internal       bool              `json:"Internal"`
	Attachable     bool              `json:"Attachable"`
	EnableIPv6     bool              `json:"EnableIPv6"`
	Options        map[string]string `json:"Options"`
	Labels         map[string]string `json:"Labels"`
	IPAM           struct {
		Driver string `json:"Driver"`
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

type networkConnectRequest struct {
	Container      string            `json:"Container"`
	EndpointConfig *EndpointSettings `json:"EndpointConfig"`
	Force          bool              `json:"Force"`
}

func (h *Handler) listNetworks(w http.ResponseWriter, r *http.Request) {
	filt := parseFilters(r)
	all := h.networksWithContainers()
	out := make([]map[string]any, 0, len(all))
	for _, n := range all {
		if filt.has("driver") {
			d, _ := n["Driver"].(string)
			if !filt.matchAny("driver", d) {
				continue
			}
		}
		if filt.has("name") {
			name, _ := n["Name"].(string)
			ok := false
			for _, want := range filt["name"] {
				if strings.Contains(name, want) {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		if filt.has("id") {
			id, _ := n["Id"].(string)
			ok := false
			for _, want := range filt["id"] {
				if strings.HasPrefix(id, want) {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		if filt.has("scope") {
			s, _ := n["Scope"].(string)
			if !filt.matchAny("scope", s) {
				continue
			}
		}
		if filt.has("label") {
			labels, _ := n["Labels"].(map[string]string)
			if !filt.matchLabel(labels) {
				continue
			}
		}
		out = append(out, n)
	}
	jsonResponse(w, http.StatusOK, out)
}

func (h *Handler) inspectNetwork(w http.ResponseWriter, r *http.Request) {
	id := canonicalNetworkName(mux.Vars(r)["id"])
	name, networkID, _, ok := h.resolveNetwork(id)
	if !ok {
		errResponse(w, http.StatusNotFound, "network not found")
		return
	}
	for _, n := range h.networksWithContainers() {
		if n["Id"] == networkID || n["Name"] == name {
			jsonResponse(w, http.StatusOK, n)
			return
		}
	}
	errResponse(w, http.StatusNotFound, "network not found")
}

// networksWithContainers returns the default network snapshot with the
// gow bridge's Containers map populated from the store. Portainer's
// Networks tab renders this as "containers attached" with links to each.
func (h *Handler) networksWithContainers() []map[string]any {
	nets := defaultNetworks()
	membersByNetwork := map[string]map[string]any{}
	for _, rec := range h.store.ListContainers() {
		attachments := rec.Networks
		if len(attachments) == 0 {
			attachments = defaultContainerNetworks(rec)
		}
		for name, attached := range attachments {
			networkID := attached.NetworkID
			if networkID == "" {
				networkID = name
			}
			if membersByNetwork[networkID] == nil {
				membersByNetwork[networkID] = map[string]any{}
			}
			ipv4 := attached.IPAddress
			if ipv4 == "" {
				ipv4 = rec.IPAddress
			}
			if ipv4 != "" {
				ipv4 += "/24"
			}
			attachedEndpointID := attached.EndpointID
			if attachedEndpointID == "" {
				attachedEndpointID = endpointID(rec.ID, name)
			}
			membersByNetwork[networkID][rec.ID] = map[string]string{
				"Name":        rec.Name,
				"EndpointID":  attachedEndpointID,
				"MacAddress":  attached.MacAddress,
				"IPv4Address": ipv4,
				"IPv6Address": "",
			}
		}
	}
	for _, n := range nets {
		id, _ := n["Id"].(string)
		if membersByNetwork[id] == nil {
			n["Containers"] = map[string]any{}
			continue
		}
		n["Containers"] = membersByNetwork[id]
	}
	for _, n := range h.store.ListNetworks() {
		containers := membersByNetwork[n.ID]
		if containers == nil {
			containers = map[string]any{}
		}
		nets = append(nets, userNetworkSnapshot(n, containers))
	}
	return nets
}

func (h *Handler) createNetwork(w http.ResponseWriter, r *http.Request) {
	var body networkCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		errResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		errResponse(w, http.StatusBadRequest, "network name cannot be empty")
		return
	}
	if isBuiltInNetwork(body.Name) || h.store.GetNetwork(body.Name) != nil {
		errResponse(w, http.StatusConflict,
			fmt.Sprintf("network with name %s already exists", body.Name))
		return
	}
	id := generateID()
	rec := &store.NetworkRecord{
		ID:         id,
		Name:       body.Name,
		Driver:     orDefault(body.Driver, "bridge"),
		Scope:      "local",
		CreatedAt:  time.Now(),
		Labels:     body.Labels,
		Options:    body.Options,
		EnableIPv6: body.EnableIPv6,
		Internal:   body.Internal,
		Attachable: body.Attachable,
		IPAMDriver: orDefault(body.IPAM.Driver, "default"),
	}
	if len(body.IPAM.Config) > 0 {
		rec.Subnet = body.IPAM.Config[0].Subnet
		rec.Gateway = body.IPAM.Config[0].Gateway
	}
	if err := h.store.AddNetwork(rec); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.emitNetwork("create", id, body.Name)
	jsonResponse(w, http.StatusCreated, map[string]string{
		"Id":      id,
		"Warning": "",
	})
}

func (h *Handler) connectNetwork(w http.ResponseWriter, r *http.Request) {
	name, id, gateway, ok := h.resolveNetwork(mux.Vars(r)["id"])
	if !ok {
		errResponse(w, http.StatusNotFound, "network not found")
		return
	}
	if name == "host" || name == "none" {
		errResponse(w, http.StatusConflict, "network cannot be connected dynamically")
		return
	}
	var body networkConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		errResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	cid := h.store.ResolveID(body.Container)
	if cid == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	rec := h.store.GetContainer(cid)
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	if len(rec.Networks) == 0 {
		rec.Networks = defaultContainerNetworks(rec)
	}
	ep := EndpointSettings{}
	if body.EndpointConfig != nil {
		ep = *body.EndpointConfig
	}
	ip := ep.IPAddress
	if ip == "" && ep.IPAMConfig != nil && ep.IPAMConfig.IPv4Address != "" {
		ip = ep.IPAMConfig.IPv4Address
	}
	if ip == "" {
		ip = rec.IPAddress
	}
	if ep.Gateway != "" {
		gateway = ep.Gateway
	}
	aliases := append([]string{}, ep.Aliases...)
	if len(aliases) == 0 {
		aliases = []string{rec.Name}
	}
	rec.Networks[name] = store.NetworkAttachment{
		NetworkID:  id,
		IPAddress:  ip,
		Gateway:    gateway,
		MacAddress: ep.MacAddress,
		EndpointID: endpointID(rec.ID, name),
		Aliases:    aliases,
		Links:      append([]string{}, ep.Links...),
		DriverOpts: copyStringMap(ep.DriverOpts),
		IPAMConfig: endpointIPAMToStore(ep.IPAMConfig),
	}
	if err := h.store.AddContainer(rec); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.emitNetworkFull("connect", id, name, cid)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) disconnectNetwork(w http.ResponseWriter, r *http.Request) {
	name, id, _, ok := h.resolveNetwork(mux.Vars(r)["id"])
	if !ok {
		errResponse(w, http.StatusNotFound, "network not found")
		return
	}
	if name == "gow" {
		errResponse(w, http.StatusConflict, "primary network cannot be disconnected")
		return
	}
	var body networkConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		errResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	cid := h.store.ResolveID(body.Container)
	if cid == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	rec := h.store.GetContainer(cid)
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	if len(rec.Networks) == 0 {
		rec.Networks = defaultContainerNetworks(rec)
	}
	if _, attached := rec.Networks[name]; !attached {
		errResponse(w, http.StatusNotFound, "container is not connected to the network")
		return
	}
	delete(rec.Networks, name)
	if err := h.store.AddContainer(rec); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.emitNetworkFull("disconnect", id, name, cid)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) knownNetwork(id string) bool {
	_, _, _, ok := h.resolveNetwork(id)
	return ok
}

func (h *Handler) removeNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	switch {
	case isBuiltInNetwork(id):
		errResponse(w, http.StatusForbidden, id+" is a pre-defined network and cannot be removed")
		return
	}
	n := h.store.GetNetwork(id)
	if n == nil {
		errResponse(w, http.StatusNotFound, "network not found")
		return
	}
	for _, rec := range h.store.ListContainers() {
		attachments := rec.Networks
		if len(attachments) == 0 {
			attachments = defaultContainerNetworks(rec)
		}
		for name, attached := range attachments {
			if name == n.Name || attached.NetworkID == n.ID {
				errResponse(w, http.StatusConflict, "network is in use")
				return
			}
		}
	}
	if err := h.store.RemoveNetwork(n.ID); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.emitNetwork("destroy", n.ID, n.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) resolveNetwork(idOrName string) (name, id, gateway string, ok bool) {
	idOrName = canonicalNetworkName(idOrName)
	if n := h.store.GetNetwork(idOrName); n != nil {
		return n.Name, n.ID, orDefault(n.Gateway, "10.100.0.1"), true
	}
	for _, n := range defaultNetworks() {
		name, _ := n["Name"].(string)
		id, _ := n["Id"].(string)
		if id == idOrName || name == idOrName {
			return name, id, networkSnapshotGateway(n), true
		}
		if len(idOrName) >= 4 && strings.HasPrefix(id, idOrName) {
			return name, id, networkSnapshotGateway(n), true
		}
	}
	return "", "", "", false
}

func isBuiltInNetwork(name string) bool {
	switch canonicalNetworkName(name) {
	case "gow", "host", "none", "bridge":
		return true
	}
	return false
}

func userNetworkSnapshot(n *store.NetworkRecord, containers map[string]any) map[string]any {
	ipamConfig := []map[string]string{}
	if n.Subnet != "" || n.Gateway != "" {
		cfg := map[string]string{}
		if n.Subnet != "" {
			cfg["Subnet"] = n.Subnet
		}
		if n.Gateway != "" {
			cfg["Gateway"] = n.Gateway
		}
		ipamConfig = append(ipamConfig, cfg)
	}
	return map[string]any{
		"Name":       n.Name,
		"Id":         n.ID,
		"Driver":     orDefault(n.Driver, "bridge"),
		"Scope":      orDefault(n.Scope, "local"),
		"EnableIPv6": n.EnableIPv6,
		"Internal":   n.Internal,
		"Attachable": n.Attachable,
		"Ingress":    false,
		"IPAM": map[string]any{
			"Driver": orDefault(n.IPAMDriver, "default"),
			"Config": ipamConfig,
		},
		"Options":    orStringMap(n.Options),
		"Labels":     orStringMap(n.Labels),
		"Containers": containers,
	}
}

func networkSnapshotGateway(n map[string]any) string {
	ipam, _ := n["IPAM"].(map[string]any)
	configs, _ := ipam["Config"].([]map[string]string)
	if len(configs) > 0 && configs[0]["Gateway"] != "" {
		return configs[0]["Gateway"]
	}
	return "10.100.0.1"
}

func orStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	return in
}

// --- system / volumes / auth stubs ---

// systemDF is the engine disk-usage summary Portainer's dashboard renders as
// "Storage used". The body structure mirrors `docker system df --format
// json` — Portainer totals the per-entity Size fields to compute the cards.
//
// Walking many rootfs directories is slow, so we short-circuit to 0 for any
// PVE-backed templates/containers (their size lives on ZFS and is cheaper to
// fetch from `zfs get used`, but that would add a shell-out per entry).
func (h *Handler) systemDF(w http.ResponseWriter, r *http.Request) {
	lxcPath := h.mgr.LXCPath()
	wanted := map[string]bool{}
	for _, t := range r.URL.Query()["type"] {
		wanted[strings.ToLower(t)] = true
	}
	all := len(wanted) == 0

	resp := map[string]any{"LayersSize": int64(0)}

	if all || wanted["image"] {
		var layersSize int64
		images := make([]map[string]any, 0)
		for _, img := range h.store.ListImages() {
			size := imageSize(lxcPath, img)
			layersSize += size
			images = append(images, map[string]any{
				"Id":          "sha256:" + img.ID,
				"RepoTags":    []string{img.Ref},
				"RepoDigests": []string{},
				"Created":     img.Created.Unix(),
				"Size":        size,
				"VirtualSize": size,
				"SharedSize":  0,
				"Containers":  -1,
			})
		}
		resp["LayersSize"] = layersSize
		resp["Images"] = images
	}

	if all || wanted["container"] {
		containers := make([]map[string]any, 0)
		for _, c := range h.store.ListContainers() {
			size := rootfsSize(h.mgr.RootfsPath(c.ID))
			containers = append(containers, map[string]any{
				"Id":         c.ID,
				"Names":      []string{"/" + c.Name},
				"Image":      c.Image,
				"ImageID":    c.ImageID,
				"Created":    c.Created.Unix(),
				"SizeRw":     size,
				"SizeRootFs": size,
				"State":      "",
				"Labels":     c.Labels,
			})
		}
		resp["Containers"] = containers
	}

	if all || wanted["volume"] {
		volumes := make([]map[string]any, 0)
		for _, v := range h.store.ListVolumes() {
			volumes = append(volumes, map[string]any{
				"Name":       v.Name,
				"Driver":     v.Driver,
				"Mountpoint": v.Mountpoint,
				"CreatedAt":  v.Created.Format(time.RFC3339Nano),
				"Scope":      "local",
				"Labels":     v.Labels,
				"UsageData": map[string]int64{
					"Size":     rootfsSize(v.Mountpoint),
					"RefCount": int64(volumeRefCount(h.store, v.Name)),
				},
			})
		}
		resp["Volumes"] = volumes
	}

	if all || wanted["build-cache"] {
		resp["BuildCache"] = []any{}
	}

	jsonResponse(w, http.StatusOK, resp)
}

// rootfsSize is a best-effort disk-usage walker for a directory. Errors on
// individual files are swallowed; the aggregate is what Portainer shows.
// Symlinks are followed for files only (we don't descend into symlinked
// directories to avoid cycles).
func rootfsSize(path string) int64 {
	if path == "" {
		return 0
	}
	var total int64
	filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// volumeRefCount returns how many stored containers mount the given volume.
// Docker exposes this on df so the UI can gate "Remove" on zero refs.
func volumeRefCount(st *store.Store, name string) int {
	v := st.GetVolume(name)
	if v == nil {
		return 0
	}
	refs := 0
	for _, c := range st.ListContainers() {
		for _, m := range c.Mounts {
			if m.Source == v.Mountpoint {
				refs++
				break
			}
		}
	}
	return refs
}

// volumeInUse returns true when any container has a mount whose source
// equals the given volume's backing directory. This is the primitive
// Docker uses to gate volume removal on active consumers.
func (h *Handler) volumeInUse(name string) bool {
	v := h.store.GetVolume(name)
	if v == nil {
		return false
	}
	for _, c := range h.store.ListContainers() {
		for _, m := range c.Mounts {
			if m.Source == v.Mountpoint {
				return true
			}
		}
	}
	return false
}

// --- helpers ---

const apiVersion = "1.43"

// defaultNetworks returns the static network snapshot reported to Docker
// clients. The daemon's real bridge is gow0 (see internal/lxc/network.go);
// host/none are synthetic entries that match Docker's built-ins so
// NetworkMode="host" and similar references resolve without errors.
func defaultNetworks() []map[string]any {
	return []map[string]any{
		{
			"Name":       "gow",
			"Id":         "gow",
			"Driver":     "bridge",
			"Scope":      "local",
			"EnableIPv6": false,
			"Internal":   false,
			"Attachable": true,
			"Ingress":    false,
			"IPAM": map[string]any{
				"Driver": "default",
				"Config": []map[string]string{
					{"Subnet": "10.100.0.0/24", "Gateway": "10.100.0.1"},
				},
			},
			"Options":    map[string]string{"com.docker.network.bridge.name": "gow0"},
			"Labels":     map[string]string{},
			"Containers": map[string]any{},
		},
		{
			"Name":   "host",
			"Id":     "host",
			"Driver": "host",
			"Scope":  "local",
			"IPAM":   map[string]any{"Driver": "default", "Config": []any{}},
		},
		{
			"Name":   "none",
			"Id":     "none",
			"Driver": "null",
			"Scope":  "local",
			"IPAM":   map[string]any{"Driver": "default", "Config": []any{}},
		},
	}
}

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
