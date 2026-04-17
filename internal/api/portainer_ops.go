package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/mux"
)

// setAttachPTY records (or clears) the PTY master for an attach session.
// Portainer's browser terminal calls /containers/{id}/resize while the
// attach is live, and resizeContainer forwards the ioctl to this PTY.
func (h *Handler) setAttachPTY(id string, p *os.File) {
	h.attachMu.Lock()
	defer h.attachMu.Unlock()
	if p == nil {
		delete(h.attachPTYs, id)
		return
	}
	h.attachPTYs[id] = p
}

func (h *Handler) getAttachPTY(id string) *os.File {
	h.attachMu.Lock()
	defer h.attachMu.Unlock()
	return h.attachPTYs[id]
}

func parseResize(r *http.Request) (rows, cols uint16, ok bool) {
	h, err1 := strconv.Atoi(r.URL.Query().Get("h"))
	w, err2 := strconv.Atoi(r.URL.Query().Get("w"))
	if err1 != nil || err2 != nil || h <= 0 || w <= 0 {
		return 0, 0, false
	}
	return uint16(h), uint16(w), true
}

// POST /containers/{id}/resize
func (h *Handler) resizeContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	rows, cols, ok := parseResize(r)
	if !ok {
		errResponse(w, http.StatusBadRequest, "invalid h/w query params")
		return
	}
	if p := h.getAttachPTY(id); p != nil {
		_ = pty.Setsize(p, &pty.Winsize{Rows: rows, Cols: cols})
	}
	w.WriteHeader(http.StatusOK)
}

// POST /exec/{id}/resize
func (h *Handler) execResize(w http.ResponseWriter, r *http.Request) {
	rec := h.execs.get(mux.Vars(r)["id"])
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such exec instance")
		return
	}
	rows, cols, ok := parseResize(r)
	if !ok {
		errResponse(w, http.StatusBadRequest, "invalid h/w query params")
		return
	}
	if rec.pty != nil {
		_ = pty.Setsize(rec.pty, &pty.Winsize{Rows: rows, Cols: cols})
	}
	w.WriteHeader(http.StatusOK)
}

// POST /containers/{id}/pause
// LXC freeze requires the freezer cgroup, which is not available for
// unprivileged containers on modern kernels. Return a clear 409 so Portainer
// surfaces a real message instead of a mystery 404.
func (h *Handler) pauseContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	errResponse(w, http.StatusConflict, "pause is not supported by docker-lxc-daemon")
}

// POST /containers/{id}/unpause
func (h *Handler) unpauseContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	errResponse(w, http.StatusConflict, "unpause is not supported by docker-lxc-daemon")
}

// POST /containers/{id}/update
// Docker's update endpoint mutates cgroup-level resource limits (CPU, memory,
// ...). We accept the request and acknowledge it so Portainer's resource
// editor completes, but we do not propagate the change — resource limits live
// in the LXC config and require a separate workflow to apply.
func (h *Handler) updateContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	jsonResponse(w, http.StatusOK, map[string]any{"Warnings": []string{}})
}

// POST /containers/prune
func (h *Handler) pruneContainers(w http.ResponseWriter, r *http.Request) {
	deleted := []string{}
	var reclaimed int64
	for _, rec := range h.store.ListContainers() {
		state, _ := h.mgr.State(rec.ID)
		if state == "running" {
			continue
		}
		if err := h.mgr.RemoveContainer(rec.ID); err != nil {
			continue
		}
		h.publishEvent("container", "destroy", rec.ID, map[string]string{
			"name":  rec.Name,
			"image": normalizeImageRef(rec.Image),
		})
		deleted = append(deleted, rec.ID)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ContainersDeleted": deleted,
		"SpaceReclaimed":    reclaimed,
	})
}

// POST /images/prune
// Portainer's prune includes a `filters` JSON blob. With
// dangling=["true"] (Docker's default) only dangling images are removed —
// we have no dangling state, so we delete nothing. With dangling=["false"]
// every image not currently referenced by a container is removed.
func (h *Handler) pruneImages(w http.ResponseWriter, r *http.Request) {
	onlyDangling := true
	if raw := r.URL.Query().Get("filters"); raw != "" {
		var filters map[string][]string
		if err := json.Unmarshal([]byte(raw), &filters); err == nil {
			if vals, ok := filters["dangling"]; ok {
				for _, v := range vals {
					if v == "false" || v == "0" {
						onlyDangling = false
					}
				}
			}
		}
	}

	deleted := []map[string]string{}
	var reclaimed int64
	if !onlyDangling {
		inUse := map[string]struct{}{}
		for _, c := range h.store.ListContainers() {
			inUse[normalizeImageRef(c.Image)] = struct{}{}
		}
		for _, img := range h.store.ListImages() {
			if _, used := inUse[img.Ref]; used {
				continue
			}
			if err := h.mgr.RemoveImage(img.Ref); err != nil {
				continue
			}
			h.publishEvent("image", "delete", img.Ref, map[string]string{"name": img.Ref})
			deleted = append(deleted, map[string]string{"Untagged": img.Ref})
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ImagesDeleted":  deleted,
		"SpaceReclaimed": reclaimed,
	})
}

// POST /commit
// Portainer's "duplicate/edit" flow snapshots a container into an image using
// this endpoint. We approximate it by creating a new image record that points
// at the source container's image — no squash, no layer history, but enough
// for Portainer to surface a new tag that can be used to recreate the
// container with the edited settings.
func (h *Handler) commitContainer(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	containerParam := q.Get("container")
	if containerParam == "" {
		errResponse(w, http.StatusBadRequest, "container query parameter is required")
		return
	}
	id := h.resolveID(containerParam)
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container: "+containerParam)
		return
	}
	repo := strings.TrimSpace(q.Get("repo"))
	tag := strings.TrimSpace(q.Get("tag"))
	if repo == "" {
		errResponse(w, http.StatusBadRequest, "repo is required")
		return
	}
	if tag == "" {
		tag = "latest"
	}
	ref := repo
	if !strings.Contains(repo, ":") {
		ref = repo + ":" + tag
	}
	ref = normalizeImageRef(ref)

	rec := h.store.GetContainer(id)
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such container: "+containerParam)
		return
	}
	src := h.store.GetImage(normalizeImageRef(rec.Image))
	if src == nil {
		errResponse(w, http.StatusConflict,
			fmt.Sprintf("container %s references unknown image %s", id, rec.Image))
		return
	}

	dup := *src
	dup.Ref = ref
	dup.Created = time.Now()
	if err := h.store.AddImage(&dup); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.publishEvent("image", "create", ref, map[string]string{"name": ref})
	jsonResponse(w, http.StatusCreated, map[string]string{
		"Id": "sha256:" + dup.ID,
	})
}
