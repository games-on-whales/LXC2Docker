package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
)

func (h *Handler) containerChanges(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	jsonResponse(w, http.StatusOK, []ChangeResponse{})
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
	memUsage, memLimit, pids := h.collectContainerUsage(id)
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
				PercpuUsage: make([]uint64, runtime.NumCPU()),
			},
			OnlineCPUs: runtime.NumCPU(),
		},
		PreCPUStats: CPUStats{
			CPUUsage: CPUUsage{
				PercpuUsage: make([]uint64, runtime.NumCPU()),
			},
			OnlineCPUs: runtime.NumCPU(),
		},
		MemoryStats: MemoryStats{
			Usage:    uint64(memUsage),
			MaxUsage: uint64(memUsage),
			Limit:    uint64(memLimit),
			Stats: map[string]any{
				"cache": 0,
			},
		},
		Networks: map[string]NetStats{
			"gow": {},
		},
	}
}

func (h *Handler) collectContainerUsage(id string) (memUsage int64, memLimit int64, pids int) {
	rec := h.store.GetContainer(id)
	if rec != nil && rec.VMID == 0 {
		if rootfsSize, err := dirSize(h.mgr.RootfsPath(id)); err == nil && rootfsSize > 0 {
			memLimit = rootfsSize
		}
	}
	if rec != nil && rec.StartedAt == nil {
		return 0, max(memLimit, 1), 0
	}
	out, err := h.mgr.Exec(id, []string{"sh", "-lc", "ps -eo rss= | awk '{s+=$1} END {print s+0}'; ps -eo pid= | wc -l"}, nil).Output()
	if err != nil {
		return 0, max(memLimit, 1), 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 0 {
		if kb, err := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64); err == nil {
			memUsage = kb * 1024
		}
	}
	if len(lines) > 1 {
		if n, err := strconv.Atoi(strings.TrimSpace(lines[1])); err == nil {
			pids = n
		}
	}
	return memUsage, max(memLimit, memUsage+1), pids
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

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (h *Handler) publishEvent(kind, action, id string, attrs map[string]string) {
	if attrs == nil {
		attrs = map[string]string{}
	}
	h.eventsHub.publish(EventMessage{
		Type:   kind,
		Action: action,
		Actor: EventActor{
			ID:         id,
			Attributes: attrs,
		},
		Scope:    "local",
		Time:     time.Now().Unix(),
		TimeNano: time.Now().UnixNano(),
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
