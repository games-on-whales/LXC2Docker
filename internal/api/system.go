package api

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
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
		Architecture:       runtime.GOARCH,
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
			IndexConfigs:                            map[string]any{},
			Mirrors:                                 []string{},
		},
		Swarm: SwarmInfo{
			LocalNodeState: "inactive",
			RemoteManagers: nil,
		},
		Plugins: PluginsInfo{
			Volume:        []string{"local"},
			Network:       []string{"bridge", "host", "none"},
			Authorization: nil,
			Log:           []string{"json-file"},
		},
		DefaultRuntime:     "lxc",
		Runtimes:           map[string]any{"lxc": map[string]string{"path": "lxc-start"}},
		LiveRestoreEnabled: true,
		Isolation:          "",
		CgroupDriver:       detectCgroupDriver(),
		CgroupVersion:      detectCgroupVersion(),
		SystemTime:         time.Now().Format(time.RFC3339Nano),
		Labels:             []string{},
		ExperimentalBuild:  false,
		HTTPProxy:          os.Getenv("HTTP_PROXY"),
		HTTPSProxy:         os.Getenv("HTTPS_PROXY"),
		NoProxy:            os.Getenv("NO_PROXY"),
		SecurityOptions:    []string{"name=seccomp,profile=default"},
		Warnings:           []string{},
	}
	jsonResponse(w, http.StatusOK, resp)
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

func (h *Handler) listNetworks(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, h.networksWithContainers())
}

func (h *Handler) inspectNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	for _, n := range h.networksWithContainers() {
		if n["Id"] == id || n["Name"] == id {
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
	members := map[string]any{}
	for _, rec := range h.store.ListContainers() {
		if rec.IPAddress == "" {
			continue
		}
		members[rec.ID] = map[string]string{
			"Name":        rec.Name,
			"EndpointID":  rec.ID,
			"MacAddress":  "",
			"IPv4Address": rec.IPAddress + "/24",
			"IPv6Address": "",
		}
	}
	for _, n := range nets {
		if n["Name"] == "gow" {
			n["Containers"] = members
		}
	}
	return nets
}

func (h *Handler) createNetwork(w http.ResponseWriter, r *http.Request) {
	// Networks are not first-class in this daemon; accept and return a
	// synthetic ID so compose stacks and Portainer create forms succeed.
	jsonResponse(w, http.StatusCreated, map[string]string{
		"Id":      "stub",
		"Warning": "",
	})
}

func (h *Handler) connectNetwork(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) disconnectNetwork(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) removeNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	// Refuse to delete the three built-in networks (matching Docker's behavior).
	switch id {
	case "gow", "host", "none", "bridge":
		errResponse(w, http.StatusForbidden, id+" is a pre-defined network and cannot be removed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) pruneNetworks(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{"NetworksDeleted": []string{}})
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
			"State":      "", // Portainer doesn't read this on df
			"Labels":     c.Labels,
		})
	}

	volumes := make([]map[string]any, 0)
	for _, v := range h.store.ListVolumes() {
		volumes = append(volumes, map[string]any{
			"Name":       v.Name,
			"Driver":     v.Driver,
			"Mountpoint": v.Mountpoint,
			"CreatedAt":  v.Created.Format(time.RFC3339),
			"Scope":      "local",
			"Labels":     v.Labels,
			"UsageData": map[string]int64{
				"Size":     rootfsSize(v.Mountpoint),
				"RefCount": int64(volumeRefCount(h.store, v.Name)),
			},
		})
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"LayersSize": layersSize,
		"Images":     images,
		"Containers": containers,
		"Volumes":    volumes,
		"BuildCache": []any{},
	})
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

// volumeRoot returns the directory holding all named volume backing
// directories. Each volume gets a subdirectory named after the volume.
func (h *Handler) volumeRoot() string {
	return filepath.Join(h.mgr.LXCPath(), "..", "docker-lxc-daemon", "volumes")
}

// listVolumes returns all known named volumes. Portainer's Volumes tab
// polls this and also uses it to populate the "Volume" dropdown on the
// container create form.
func (h *Handler) listVolumes(w http.ResponseWriter, r *http.Request) {
	filt := parseFilters(r)
	records := h.store.ListVolumes()
	out := make([]map[string]any, 0, len(records))
	for _, v := range records {
		if !filt.matchLabel(v.Labels) {
			continue
		}
		if !matchVolumeName(filt, v.Name) {
			continue
		}
		if !matchVolumeDriver(filt, v.Driver) {
			continue
		}
		if filt.has("dangling") && !matchDangling(filt, h.volumeInUse(v.Name)) {
			continue
		}
		out = append(out, volumeJSON(v))
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"Volumes":  out,
		"Warnings": []string{},
	})
}

func matchVolumeName(f filters, name string) bool {
	if !f.has("name") {
		return true
	}
	for _, want := range f["name"] {
		if strings.Contains(name, want) {
			return true
		}
	}
	return false
}

func matchVolumeDriver(f filters, driver string) bool {
	if !f.has("driver") {
		return true
	}
	return f.matchAny("driver", driver)
}

func matchDangling(f filters, inUse bool) bool {
	for _, want := range f["dangling"] {
		switch want {
		case "1", "true":
			if !inUse {
				return true
			}
		case "0", "false":
			if inUse {
				return true
			}
		}
	}
	return false
}

func (h *Handler) inspectVolume(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	v := h.store.GetVolume(name)
	if v == nil {
		errResponse(w, http.StatusNotFound, "no such volume")
		return
	}
	jsonResponse(w, http.StatusOK, volumeJSON(v))
}

// createVolume persists a new named volume and mkdirs the backing directory.
// Idempotent: Portainer compose stacks issue the same POST on every up, so a
// pre-existing volume returns 201 with the existing record rather than an
// error.
func (h *Handler) createVolume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		DriverOpts map[string]string `json:"DriverOpts"`
		Labels     map[string]string `json:"Labels"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Name == "" {
		req.Name = generateVolumeName()
	}
	if req.Driver == "" {
		req.Driver = "local"
	}
	mountpoint := filepath.Join(h.volumeRoot(), req.Name)
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	v := &store.VolumeRecord{
		Name:       req.Name,
		Driver:     req.Driver,
		Mountpoint: mountpoint,
		Created:    time.Now(),
		Labels:     req.Labels,
		Options:    req.DriverOpts,
	}
	// Preserve the original creation time on re-create so Portainer's
	// "Created X ago" doesn't reset every compose-up.
	if existing := h.store.GetVolume(req.Name); existing != nil {
		v.Created = existing.Created
	}
	if err := h.store.AddVolume(v); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusCreated, volumeJSON(v))
}

func (h *Handler) removeVolume(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	v := h.store.GetVolume(name)
	if v == nil {
		// Docker returns 204 for a missing volume only when force=1; else
		// 404. Portainer doesn't rely on either behavior, so we follow the
		// stricter spec.
		if r.URL.Query().Get("force") == "1" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		errResponse(w, http.StatusNotFound, "no such volume")
		return
	}
	// Refuse to delete volumes that are actively mounted. This mirrors
	// Docker's "volume in use" error.
	if h.volumeInUse(name) && r.URL.Query().Get("force") != "1" {
		errResponse(w, http.StatusConflict, "volume is in use")
		return
	}
	os.RemoveAll(v.Mountpoint)
	h.store.RemoveVolume(name)
	w.WriteHeader(http.StatusNoContent)
}

// pruneVolumes removes volumes with no current consumers. Docker also
// respects label filters here; we implement the `label=` form via the
// filter helper.
func (h *Handler) pruneVolumes(w http.ResponseWriter, r *http.Request) {
	filt := parseFilters(r)
	var deleted []string
	for _, v := range h.store.ListVolumes() {
		if h.volumeInUse(v.Name) {
			continue
		}
		if !filt.matchLabel(v.Labels) {
			continue
		}
		os.RemoveAll(v.Mountpoint)
		h.store.RemoveVolume(v.Name)
		deleted = append(deleted, v.Name)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"VolumesDeleted": deleted,
		"SpaceReclaimed": 0,
	})
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

// volumeJSON renders a store.VolumeRecord as Docker's volume response body.
func volumeJSON(v *store.VolumeRecord) map[string]any {
	opts := v.Options
	if opts == nil {
		opts = map[string]string{}
	}
	labels := v.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return map[string]any{
		"Name":       v.Name,
		"Driver":     v.Driver,
		"Mountpoint": v.Mountpoint,
		"CreatedAt":  v.Created.Format(time.RFC3339),
		"Scope":      "local",
		"Options":    opts,
		"Labels":     labels,
	}
}

// generateVolumeName produces a 64-char hex identifier for anonymous
// volumes, matching Docker's convention.
func generateVolumeName() string {
	return generateID()
}

// auth accepts registry credentials. We don't actually authenticate (skopeo
// handles that per-pull), but Portainer posts to /auth when the user saves a
// registry; returning success lets the UI flow complete.
func (h *Handler) auth(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]string{
		"Status":        "Login Succeeded",
		"IdentityToken": "",
	})
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
