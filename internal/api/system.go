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
	w.Header().Set("API-Version", apiVersion)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
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

	resp := InfoResponse{
		ID:                "docker-lxc-daemon",
		Containers:        len(containers),
		ContainersRunning: running,
		ContainersStopped: len(containers) - running,
		Images:            len(images),
		Driver:            "lxc",
		MemoryLimit:       true,
		SwapLimit:         true,
		KernelVersion:     unameRelease(uname),
		OperatingSystem:   "Linux",
		OSType:            "linux",
		Architecture:      runtime.GOARCH,
		NCPU:              runtime.NumCPU(),
		MemTotal:          int64(si.Totalram) * int64(si.Unit),
		DockerRootDir:     h.mgr.LXCPath(),
		ServerVersion:     "24.0.0",
	}
	jsonResponse(w, http.StatusOK, resp)
}

// --- network stubs (Docker clients query networks when creating containers) ---
func (h *Handler) listNetworks(w http.ResponseWriter, r *http.Request) {
	networks := []NetworkResource{h.defaultNetworkResource()}
	for _, n := range h.store.ListNetworks() {
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
		jsonResponse(w, http.StatusOK, h.networkResource(n))
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
	id := generateID()[:12]
	rec := &store.NetworkRecord{
		ID:        id,
		Name:      req.Name,
		Driver:    orDefault(req.Driver, "bridge"),
		Scope:     "local",
		CreatedAt: time.Now().UTC(),
		Labels:    req.Labels,
		Options:   req.Options,
	}
	if err := h.store.AddNetwork(rec); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.publishEvent("network", "create", rec.ID, map[string]string{"name": rec.Name, "type": rec.Driver})
	jsonResponse(w, http.StatusCreated, NetworkCreateResponse{ID: rec.ID})
}

func (h *Handler) connectNetwork(w http.ResponseWriter, r *http.Request) {
	if !h.networkExists(mux.Vars(r)["id"]) {
		errResponse(w, http.StatusNotFound, "network not found")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) disconnectNetwork(w http.ResponseWriter, r *http.Request) {
	if !h.networkExists(mux.Vars(r)["id"]) {
		errResponse(w, http.StatusNotFound, "network not found")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// events implements GET /events and streams daemon lifecycle events using the
// Docker-compatible JSON event stream shape.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	ch := h.eventsHub.subscribe()
	defer h.eventsHub.unsubscribe(ch)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case evt := <-ch:
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

	for _, img := range images {
		repo, tag := splitImageRef(img.Ref)
		out.Images = append(out.Images, ImageUsage{
			ID:         "sha256:" + img.ID,
			Repository: repo,
			Tag:        tag,
			CreatedAt:  img.Created.Format(time.RFC3339),
			RepoTags:   []string{img.Ref},
		})
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
		out.LayersSize += size
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
		IPAM: map[string]any{
			"Driver": "default",
			"Config": []map[string]string{},
		},
		Options: n.Options,
		Labels:  n.Labels,
	}
}

func (h *Handler) networkContainers() map[string]NetworkEndpoint {
	out := map[string]NetworkEndpoint{}
	for _, c := range h.store.ListContainers() {
		if c.IPAddress == "" {
			continue
		}
		out[c.ID] = NetworkEndpoint{
			Name:        c.Name,
			EndpointID:  c.ID[:12],
			IPv4Address: c.IPAddress + "/24",
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
