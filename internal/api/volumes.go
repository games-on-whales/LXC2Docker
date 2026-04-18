package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
)

func (h *Handler) listVolumes(w http.ResponseWriter, r *http.Request) {
	filters, err := parseListFilters(r.URL.Query().Get("filters"))
	if err != nil {
		errResponse(w, http.StatusBadRequest, "invalid filters: "+err.Error())
		return
	}
	// Pre-compute refCount per volume so the filter can honour dangling=true.
	refCount := map[string]int{}
	for _, c := range h.store.ListContainers() {
		for _, m := range c.Mounts {
			if m.Name != "" {
				refCount[m.Name]++
			}
		}
	}
	vols := h.store.ListVolumes()
	out := make([]VolumeUsage, 0, len(vols))
	for _, v := range vols {
		if !matchesVolumeFilters(v, refCount[v.Name], filters) {
			continue
		}
		size, _ := dirSize(v.Mountpoint)
		out = append(out, volumeUsage(h.store, v, size))
	}
	jsonResponse(w, http.StatusOK, VolumeListResponse{
		Volumes:  out,
		Warnings: []string{},
	})
}

func (h *Handler) createVolume(w http.ResponseWriter, r *http.Request) {
	var req VolumeCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		errResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" {
		req.Name = generateID()[:12]
	}
	if existing := h.store.GetVolume(req.Name); existing != nil {
		jsonResponse(w, http.StatusCreated, volumeCreateResponse(existing))
		return
	}
	v, err := h.ensureVolume(req.Name)
	if err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	v.Driver = orDefault(req.Driver, "local")
	v.Labels = normalizeStringMap(req.Labels)
	v.Options = normalizeStringMap(req.DriverOpts)
	if err := h.store.AddVolume(v); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusCreated, volumeCreateResponse(v))
}

func (h *Handler) inspectVolume(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	v := h.store.GetVolume(name)
	if v == nil {
		errResponse(w, http.StatusNotFound, "no such volume")
		return
	}
	// Return the richer VolumeUsage shape (same fields as /volumes plus
	// UsageData) so Portainer's volume detail page can show size and
	// reference count instead of "—".
	size, _ := dirSize(v.Mountpoint)
	jsonResponse(w, http.StatusOK, volumeUsage(h.store, v, size))
}

func (h *Handler) removeVolume(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	v := h.store.GetVolume(name)
	if v == nil {
		errResponse(w, http.StatusNotFound, "no such volume")
		return
	}
	for _, c := range h.store.ListContainers() {
		for _, m := range c.Mounts {
			if m.Name == name {
				errResponse(w, http.StatusConflict, "volume is in use")
				return
			}
		}
	}
	if err := os.RemoveAll(v.Mountpoint); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.store.RemoveVolume(name); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.publishEvent("volume", "destroy", name, map[string]string{"name": name, "driver": v.Driver})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) pruneVolumes(w http.ResponseWriter, r *http.Request) {
	filters, err := parseListFilters(r.URL.Query().Get("filters"))
	if err != nil {
		errResponse(w, http.StatusBadRequest, "invalid filters: "+err.Error())
		return
	}
	deleted := []string{}
	space := int64(0)
	inUse := map[string]bool{}
	for _, c := range h.store.ListContainers() {
		for _, m := range c.Mounts {
			if m.Name != "" {
				inUse[m.Name] = true
			}
		}
	}
	for _, v := range h.store.ListVolumes() {
		if inUse[v.Name] {
			continue
		}
		// Volumes carry no created-at field the caller can filter on, so
		// until is a no-op for them; label still applies.
		if !pruneEligible(v.CreatedAt, v.Labels, filters, nil) {
			continue
		}
		size, _ := dirSize(v.Mountpoint)
		if err := os.RemoveAll(v.Mountpoint); err != nil {
			continue
		}
		if err := h.store.RemoveVolume(v.Name); err != nil {
			continue
		}
		deleted = append(deleted, v.Name)
		space += size
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"VolumesDeleted": deleted,
		"SpaceReclaimed": space,
	})
}

func volumeCreateResponse(v *store.VolumeRecord) VolumeCreateResponse {
	return VolumeCreateResponse{
		Name:       v.Name,
		Driver:     orDefault(v.Driver, "local"),
		Mountpoint: v.Mountpoint,
		CreatedAt:  v.CreatedAt.Format(time.RFC3339),
		Labels:     normalizeStringMap(v.Labels),
		Options:    normalizeStringMap(v.Options),
		Status:     map[string]string{},
		Scope:      "local",
	}
}

func volumeUsage(st *store.Store, v *store.VolumeRecord, size int64) VolumeUsage {
	refCount := 0
	for _, c := range st.ListContainers() {
		for _, m := range c.Mounts {
			if m.Name == v.Name {
				refCount++
				break
			}
		}
	}
	return VolumeUsage{
		Name:       v.Name,
		Driver:     orDefault(v.Driver, "local"),
		Mountpoint: v.Mountpoint,
		CreatedAt:  v.CreatedAt.Format(time.RFC3339),
		Labels:     normalizeStringMap(v.Labels),
		Options:    normalizeStringMap(v.Options),
		Status:     map[string]string{},
		Scope:      "local",
		UsageData: VolumeUsageData{
			RefCount: refCount,
			Size:     size,
		},
	}
}

func normalizeStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	return in
}
