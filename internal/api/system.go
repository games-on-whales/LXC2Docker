package api

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"

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

// --- network stubs (Wolf queries networks when creating containers) ---

func (h *Handler) listNetworks(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, []any{})
}

func (h *Handler) inspectNetwork(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotFound, "network not found")
}

func (h *Handler) createNetwork(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusCreated, map[string]string{"Id": "stub"})
}

func (h *Handler) connectNetwork(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) disconnectNetwork(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// events implements GET /events. It holds the connection open as a streaming
// endpoint. Wolf uses this to monitor container lifecycle events. We don't
// emit real events yet — the handler simply blocks until the client disconnects.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// Block until the client closes the connection.
	<-r.Context().Done()
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
