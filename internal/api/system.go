package api

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

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
	jsonResponse(w, http.StatusOK, defaultNetworks())
}

func (h *Handler) inspectNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	for _, n := range defaultNetworks() {
		if n["Id"] == id || n["Name"] == id {
			jsonResponse(w, http.StatusOK, n)
			return
		}
	}
	errResponse(w, http.StatusNotFound, "network not found")
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
// "Storage used". Empty arrays are fine as long as the top-level keys exist.
func (h *Handler) systemDF(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"LayersSize": 0,
		"Images":     []any{},
		"Containers": []any{},
		"Volumes":    []any{},
		"BuildCache": []any{},
	})
}

// listVolumes returns an empty volume set. The daemon exposes bind mounts
// rather than named volumes, but Portainer polls this endpoint every refresh
// and a 404 surfaces as an error banner.
func (h *Handler) listVolumes(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"Volumes":  []any{},
		"Warnings": nil,
	})
}

func (h *Handler) inspectVolume(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotFound, "no such volume")
}

func (h *Handler) createVolume(w http.ResponseWriter, r *http.Request) {
	// Accept the body but don't persist anything — the daemon has no named
	// volume store. Returning the input name keeps compose/Portainer happy.
	var req struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		DriverOpts map[string]string `json:"DriverOpts"`
		Labels     map[string]string `json:"Labels"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Name == "" {
		req.Name = "unnamed"
	}
	if req.Driver == "" {
		req.Driver = "local"
	}
	jsonResponse(w, http.StatusCreated, map[string]any{
		"Name":       req.Name,
		"Driver":     req.Driver,
		"Mountpoint": "/var/lib/docker-lxc-daemon/volumes/" + req.Name,
		"CreatedAt":  time.Now().Format(time.RFC3339),
		"Scope":      "local",
		"Options":    req.DriverOpts,
		"Labels":     req.Labels,
	})
}

func (h *Handler) removeVolume(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) pruneVolumes(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"VolumesDeleted": []string{},
		"SpaceReclaimed": 0,
	})
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
