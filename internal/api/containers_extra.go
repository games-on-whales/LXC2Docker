package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
	"golang.org/x/sys/unix"
)

func (h *Handler) containerChanges(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}

	rec := h.store.GetContainer(id)
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}

	rootfs := h.mgr.RootfsPath(id)
	base := h.mgr.ImageRootfsPath(normalizeImageRef(rec.Image))
	if rootfs == "" || base == "" {
		jsonResponse(w, http.StatusOK, []ChangeResponse{})
		return
	}

	changes, err := diffRootfs(base, rootfs)
	if err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, changes)
}

func (h *Handler) containerStats(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	stream := r.URL.Query().Get("stream")
	if stream == "" || stream == "1" || strings.EqualFold(stream, "true") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			stats := h.snapshotContainerStats(id)
			if err := enc.Encode(stats); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	}
	jsonResponse(w, http.StatusOK, h.snapshotContainerStats(id))
}

func (h *Handler) snapshotContainerStats(id string) ContainerStats {
	now := time.Now().UTC()
	cg, _ := h.mgr.CgroupPath(id)
	memUsage := readUint64(filepath.Join("/sys/fs/cgroup", cg, "memory.current"))
	memLimit := readUint64(filepath.Join("/sys/fs/cgroup", cg, "memory.max"))
	if memLimit == 0 || memLimit == ^uint64(0) {
		memLimit = systemMemTotal()
	}
	pids := int(readUint64(filepath.Join("/sys/fs/cgroup", cg, "pids.current")))
	cpuTotal, cpuUser, cpuKernel := readCPUStat(filepath.Join("/sys/fs/cgroup", cg, "cpu.stat"))
	systemCPU := readSystemCPUUsage()

	return ContainerStats{
		Read:    now.Format(time.RFC3339Nano),
		PreRead: now.Add(-time.Second).Format(time.RFC3339Nano),
		PidsStats: PidsStats{
			Current: pids,
		},
		BlkioStats:   map[string][]any{},
		NumProcs:     pids,
		StorageStats: map[string]any{},
		CPUStats: CPUStats{
			CPUUsage: CPUUsage{
				TotalUsage:        cpuTotal,
				PercpuUsage:       make([]uint64, runtime.NumCPU()),
				UsageInKernelmode: cpuKernel,
				UsageInUsermode:   cpuUser,
			},
			SystemCPUUsage: systemCPU,
			OnlineCPUs:     runtime.NumCPU(),
		},
		PreCPUStats: CPUStats{
			CPUUsage: CPUUsage{
				PercpuUsage: make([]uint64, runtime.NumCPU()),
			},
			OnlineCPUs: runtime.NumCPU(),
		},
		MemoryStats: MemoryStats{
			Usage:    memUsage,
			MaxUsage: memUsage,
			Limit:    memLimit,
			Stats: map[string]any{
				"cache": readUint64(filepath.Join("/sys/fs/cgroup", cg, "memory.stat.cache")),
			},
		},
		Networks: h.snapshotNetStats(id),
	}
}

// snapshotNetStats reads /proc/<pid>/net/dev inside the container's network
// namespace (pid lookup via lxc-info) and returns a per-interface stats map
// matching Docker's shape. The loopback interface is dropped so Portainer's
// charts focus on externally-visible traffic; a container with no running
// init (pid<=0) returns an empty map.
func (h *Handler) snapshotNetStats(id string) map[string]NetStats {
	out := map[string]NetStats{}
	pid, err := h.mgr.InitPID(id)
	if err != nil || pid <= 0 {
		return out
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/dev", pid))
	if err != nil {
		return out
	}
	for i, line := range strings.Split(string(data), "\n") {
		if i < 2 {
			// Skip the two-line header.
			continue
		}
		name, raw, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "lo" {
			continue
		}
		fields := strings.Fields(raw)
		if len(fields) < 16 {
			continue
		}
		out[name] = NetStats{
			RxBytes:   parseUint64(fields[0]),
			RxPackets: parseUint64(fields[1]),
			RxErrors:  parseUint64(fields[2]),
			RxDropped: parseUint64(fields[3]),
			TxBytes:   parseUint64(fields[8]),
			TxPackets: parseUint64(fields[9]),
			TxErrors:  parseUint64(fields[10]),
			TxDropped: parseUint64(fields[11]),
		}
	}
	return out
}

func parseUint64(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

func mountTypeForSource(st *store.Store, source string) string {
	if source == "" {
		return "bind"
	}
	for _, v := range st.ListVolumes() {
		if v.Mountpoint == source {
			return "volume"
		}
	}
	return "bind"
}

func volumeNameForSource(st *store.Store, source string) string {
	for _, v := range st.ListVolumes() {
		if v.Mountpoint == source {
			return v.Name
		}
	}
	return ""
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func (h *Handler) publishEvent(kind, action, id string, attrs map[string]string) {
	if attrs == nil {
		attrs = map[string]string{}
	}
	now := time.Now()
	h.eventsHub.publish(EventMessage{
		Type:   kind,
		Action: action,
		Actor: EventActor{
			ID:         id,
			Attributes: attrs,
		},
		Scope:    "local",
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
	})
}

func (h *Handler) ensureVolume(name string) (*store.VolumeRecord, error) {
	if name == "" {
		return nil, fmt.Errorf("volume name is required")
	}
	if existing := h.store.GetVolume(name); existing != nil {
		if err := os.MkdirAll(existing.Mountpoint, 0o755); err != nil {
			return nil, err
		}
		return existing, nil
	}
	mp := filepath.Join(h.store.RootDir(), "volumes", name)
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return nil, err
	}
	v := &store.VolumeRecord{
		Name:       name,
		Driver:     "local",
		Mountpoint: mp,
		CreatedAt:  time.Now().UTC(),
		Labels:     map[string]string{},
		Options:    map[string]string{},
	}
	if err := h.store.AddVolume(v); err != nil {
		return nil, err
	}
	h.publishEvent("volume", "create", name, map[string]string{"name": name, "driver": "local"})
	return v, nil
}

func defaultContainerNetworks(rec *store.ContainerRecord) map[string]store.NetworkAttachment {
	return map[string]store.NetworkAttachment{
		"gow": {
			NetworkID:  "gow",
			IPAddress:  rec.IPAddress,
			Gateway:    "10.100.0.1",
			EndpointID: endpointID(rec.ID, "gow"),
		},
	}
}

func attachRequestedNetworks(st *store.Store, rec *store.ContainerRecord, cfg NetworkingConfig) error {
	if len(cfg.EndpointsConfig) == 0 {
		return nil
	}
	if rec.Networks == nil {
		rec.Networks = defaultContainerNetworks(rec)
	}
	for name, ep := range cfg.EndpointsConfig {
		networkName := name
		networkID := name
		if name != "gow" {
			n := st.GetNetwork(name)
			if n == nil {
				return fmt.Errorf("network %q not found", name)
			}
			networkName = n.Name
			networkID = n.ID
		}
		rec.Networks[networkName] = store.NetworkAttachment{
			NetworkID:  networkID,
			IPAddress:  orDefault(ep.IPAddress, rec.IPAddress),
			Gateway:    orDefault(ep.Gateway, "10.100.0.1"),
			MacAddress: ep.MacAddress,
			EndpointID: endpointID(rec.ID, networkName),
		}
	}
	return nil
}

func buildContainerEndpoints(rec *store.ContainerRecord) map[string]EndpointSettings {
	if len(rec.Networks) == 0 {
		rec.Networks = defaultContainerNetworks(rec)
	}
	out := make(map[string]EndpointSettings, len(rec.Networks))
	for name, attached := range rec.Networks {
		out[name] = EndpointSettings{
			IPAddress:  attached.IPAddress,
			Gateway:    attached.Gateway,
			MacAddress: attached.MacAddress,
			NetworkID:  attached.NetworkID,
			EndpointID: attached.EndpointID,
		}
	}
	return out
}

func endpointID(containerID, networkName string) string {
	id := containerID
	if len(id) > 12 {
		id = id[:12]
	}
	suffix := strings.ReplaceAll(networkName, "/", "_")
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	return id + "-" + suffix
}

func diffRootfs(baseRoot, currentRoot string) ([]ChangeResponse, error) {
	baseEntries, err := walkRootfs(baseRoot)
	if err != nil {
		return nil, err
	}
	currentEntries, err := walkRootfs(currentRoot)
	if err != nil {
		return nil, err
	}

	changes := make([]ChangeResponse, 0)
	for path, base := range baseEntries {
		current, ok := currentEntries[path]
		if !ok {
			changes = append(changes, ChangeResponse{Path: path, Kind: 2})
			continue
		}
		if fileChanged(base, current) {
			changes = append(changes, ChangeResponse{Path: path, Kind: 0})
		}
	}
	for path := range currentEntries {
		if _, ok := baseEntries[path]; !ok {
			changes = append(changes, ChangeResponse{Path: path, Kind: 1})
		}
	}
	return changes, nil
}

type fileSnapshot struct {
	mode    fs.FileMode
	size    int64
	modTime time.Time
	link    string
}

func walkRootfs(root string) (map[string]fileSnapshot, error) {
	out := map[string]fileSnapshot{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := "/" + filepath.ToSlash(rel)
		snap := fileSnapshot{
			mode:    info.Mode(),
			size:    info.Size(),
			modTime: info.ModTime().UTC().Truncate(time.Second),
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(path); err == nil {
				snap.link = target
			}
		}
		out[key] = snap
		return nil
	})
	return out, err
}

func fileChanged(a, b fileSnapshot) bool {
	if a.mode.Type() != b.mode.Type() {
		return true
	}
	if a.mode.Perm() != b.mode.Perm() {
		return true
	}
	if a.mode&os.ModeSymlink != 0 {
		return a.link != b.link
	}
	if a.mode.IsRegular() && (a.size != b.size || !a.modTime.Equal(b.modTime)) {
		return true
	}
	return false
}

func readUint64(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(data))
	if s == "" || s == "max" {
		return ^uint64(0)
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func readCPUStat(path string) (total, user, system uint64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "usage_usec":
			total = v * 1000
		case "user_usec":
			user = v * 1000
		case "system_usec":
			system = v * 1000
		}
	}
	return total, user, system
}

func readSystemCPUUsage() uint64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var total uint64
		for _, field := range fields {
			v, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				continue
			}
			total += v
		}
		ticks := uint64(100)
		return total * uint64(time.Second) / ticks
	}
	return 0
}

func systemMemTotal() uint64 {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return 0
	}
	return uint64(si.Totalram) * uint64(si.Unit)
}
