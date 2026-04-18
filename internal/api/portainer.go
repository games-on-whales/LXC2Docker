// Portainer-centric handlers. These endpoints don't change the daemon's
// underlying model — they round out the Docker API surface that web UIs
// (Portainer, LazyDocker, etc.) poll on top of the core container endpoints.
package api

import (
	"io"
	"net/http"

	"github.com/gorilla/mux"
)

// POST /containers/{id}/pause
func (h *Handler) pauseContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	if err := h.mgr.PauseContainer(id); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.emitContainer("pause", h.store.GetContainer(id))
	w.WriteHeader(http.StatusNoContent)
}

// POST /containers/{id}/unpause
func (h *Handler) unpauseContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	if err := h.mgr.UnpauseContainer(id); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.emitContainer("unpause", h.store.GetContainer(id))
	w.WriteHeader(http.StatusNoContent)
}

// GET /containers/{id}/changes
//
// Docker returns a filesystem diff against the image layer. LXC containers
// don't layer their rootfs, so there is nothing to diff. Returning an empty
// array (not null) keeps Portainer's "Changes" tab functional without
// claiming changes that aren't there.
func (h *Handler) containerChanges(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	jsonResponse(w, http.StatusOK, []any{})
}

// POST /containers/{id}/resize?h=<rows>&w=<cols>
//
// Docker's CLI and Portainer's web terminal POST to this when the client
// window changes size. We accept and discard — the lxc-attach PTY sizes off
// the parent terminal, and Portainer's XTerm.js session uses exec instead.
func (h *Handler) resizeContainer(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// POST /exec/{id}/resize?h=<rows>&w=<cols>
//
// Real Docker forwards a TIOCSWINSZ ioctl to the exec PTY. We don't persist
// the PTY beyond the hijacked connection, so the ioctl would arrive too
// late. Acking 200 is enough to silence Portainer's per-keystroke retries.
func (h *Handler) resizeExec(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// POST /containers/prune
//
// Portainer's "Remove unused" button calls this. We iterate stopped
// non-ephemeral containers and remove them. Filters are ignored because the
// daemon has no label-filtering on prune yet.
func (h *Handler) pruneContainers(w http.ResponseWriter, r *http.Request) {
	var deleted []string
	for _, rec := range h.store.ListContainers() {
		state, _ := h.mgr.State(rec.ID)
		if state != "exited" && state != "created" {
			continue
		}
		if err := h.mgr.RemoveContainer(rec.ID); err == nil {
			deleted = append(deleted, rec.ID)
			h.emitContainer("destroy", rec)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ContainersDeleted": deleted,
		"SpaceReclaimed":    0,
	})
}

// POST /images/prune
//
// The daemon doesn't track image refcounts, so "dangling" doesn't apply. We
// leave the set untouched and report no work done. Acking prevents the UI
// from showing an error.
func (h *Handler) pruneImages(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"ImagesDeleted":  []any{},
		"SpaceReclaimed": 0,
	})
}

// GET /images/{name}/history
//
// LXC templates are a single flattened rootfs, so "history" is one virtual
// layer. Return a single entry that reflects the image metadata the UI has.
func (h *Handler) imageHistory(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	rec := h.store.GetImage(normalizeImageRef(name))
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such image: "+name)
		return
	}
	jsonResponse(w, http.StatusOK, []map[string]any{
		{
			"Id":        "sha256:" + rec.ID,
			"Created":   rec.Created.Unix(),
			"CreatedBy": "lxc-template " + rec.TemplateName,
			"Tags":      []string{rec.Ref},
			"Size":      0,
			"Comment":   "Flattened LXC rootfs",
		},
	})
}

// POST /images/{name}/tag?repo=<repo>&tag=<tag>
//
// Portainer's image detail "Tag" button adds an additional ref to an existing
// image. The daemon's store keys images by ref, so we add a second entry
// pointing at the same template. Missing source returns 404, duplicate
// target is silently treated as idempotent.
func (h *Handler) tagImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	src := h.store.GetImage(normalizeImageRef(name))
	if src == nil {
		errResponse(w, http.StatusNotFound, "No such image: "+name)
		return
	}
	repo := r.URL.Query().Get("repo")
	tag := r.URL.Query().Get("tag")
	if repo == "" {
		errResponse(w, http.StatusBadRequest, "repo is required")
		return
	}
	if tag == "" {
		tag = "latest"
	}
	newRef := repo + ":" + tag

	// Copy the record under the new ref. The template container is shared,
	// so subsequent container creates can use either ref interchangeably.
	cp := *src
	cp.Ref = newRef
	if err := h.store.AddImage(&cp); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// POST /containers/{id}/update
//
// Portainer exposes a "Resource limits" modal that POSTs updated Memory,
// CPU, and restart policy fields. LXC can't change most of these without a
// restart, so we accept the request, update the stored HostConfig so
// inspect reflects the new values, and return an empty warnings list.
func (h *Handler) updateContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	// The body shape is HostConfig plus an optional RestartPolicy override.
	body, _ := io.ReadAll(r.Body)
	rec := h.store.GetContainer(id)
	if rec != nil && len(body) > 0 {
		// Merge the posted fields into the stored HostConfig so inspect
		// shows the new values. We deliberately don't try to enforce them
		// on the running container — restart policy, in particular, is a
		// daemon-level concept this codebase doesn't implement.
		rec.RawHostConfig = body
		_ = h.store.AddContainer(rec)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"Warnings": []string{},
	})
}

// GET /distribution/{name}/json
//
// Portainer's "Pull image" modal probes this to decide whether a pull is
// needed and what architectures a remote manifest offers. We don't hit the
// registry ourselves; a minimal response with a single linux/amd64 platform
// entry is enough to pass the UI's "does this image exist remotely?" check.
func (h *Handler) distributionInspect(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"Descriptor": map[string]any{
			"MediaType": "application/vnd.docker.distribution.manifest.v2+json",
			"Digest":    "",
			"Size":      0,
			"URLs":      []string{},
		},
		"Platforms": []map[string]string{
			{"architecture": "amd64", "os": "linux"},
		},
	})
}
