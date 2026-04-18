// Portainer-centric handlers. These endpoints don't change the daemon's
// underlying model — they round out the Docker API surface that web UIs
// (Portainer, LazyDocker, etc.) poll on top of the core container endpoints.
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/creack/pty"
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
// Forwards TIOCSWINSZ to the live exec PTY so web terminals render at the
// client's window size. When the exec isn't running (or isn't a TTY), we
// still return 200 — clients retry on resize regardless and 4xx/5xx
// surfaces as an error toast in Portainer.
func (h *Handler) resizeExec(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	rec := h.execs.get(id)
	if rec == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	rows, cols := parseResize(r)
	if rec.Pty != nil && rows > 0 && cols > 0 {
		_ = pty.Setsize(rec.Pty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	}
	w.WriteHeader(http.StatusOK)
}

// parseResize pulls h=<rows> / w=<cols> (Docker's wire names) from either
// query string or form body. Returns (0,0) for malformed input.
func parseResize(r *http.Request) (rows, cols int) {
	q := r.URL.Query()
	rows, _ = strconv.Atoi(q.Get("h"))
	cols, _ = strconv.Atoi(q.Get("w"))
	return
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
	h.emitImage("tag", newRef)
	w.WriteHeader(http.StatusCreated)
}

// POST /containers/{id}/update
//
// Portainer exposes a "Resource limits" modal that POSTs updated Memory,
// CPU, and restart policy fields. We persist the new values so inspect
// reflects them, and — when the container is running — push the memory /
// CPU changes into the live cgroup. Changes that require a restart
// (capabilities, namespaces) apply on next start.
func (h *Handler) updateContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var posted HostConfig
	if len(body) > 0 {
		_ = json.Unmarshal(body, &posted)
	}

	rec := h.store.GetContainer(id)
	warnings := []string{}
	if rec != nil {
		if len(body) > 0 {
			rec.RawHostConfig = body
		}
		// Keep the hoisted restart policy fields in sync.
		if posted.RestartPolicy.Name != "" {
			rec.RestartPolicy = posted.RestartPolicy.Name
			rec.RestartMaxRetry = posted.RestartPolicy.MaximumRetryCount
		}
		_ = h.store.AddContainer(rec)

		// Apply live cgroup changes for running containers. Errors here
		// are non-fatal — Portainer treats warnings as advisories.
		if state, _ := h.mgr.State(id); state == "running" {
			if w := applyLiveLimits(id, posted); w != "" {
				warnings = append(warnings, w)
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"Warnings": warnings,
	})
}

// applyLiveLimits writes Memory/CPU limits to the container's cgroup so
// Portainer's "update resource limits" modal takes effect without a
// restart. Returns a non-empty warning string when any write failed.
func applyLiveLimits(id string, hc HostConfig) string {
	cg := resolveCgroupPath(id, containerPID(id))
	if cg == "" {
		return "cgroup not found; limits applied on next start"
	}
	writes := map[string]string{}
	if hc.Memory > 0 {
		writes["memory.max"] = strconv.FormatInt(hc.Memory, 10)
	}
	if hc.CPUShares > 0 {
		// Docker shares (1–1024) → cgroup v2 weight (1–10000).
		weight := (hc.CPUShares * 10000) / 1024
		if weight < 1 {
			weight = 1
		}
		writes["cpu.weight"] = strconv.FormatInt(weight, 10)
	}
	if hc.CPUQuota > 0 {
		period := hc.CPUPeriod
		if period <= 0 {
			period = 100000
		}
		writes["cpu.max"] = strconv.FormatInt(hc.CPUQuota, 10) + " " + strconv.FormatInt(period, 10)
	} else if hc.NanoCPUs > 0 {
		// 1 CPU = 1e9 NanoCPUs → quota µs at 100 ms period.
		quota := hc.NanoCPUs * 100000 / 1_000_000_000
		if quota < 1000 {
			quota = 1000
		}
		writes["cpu.max"] = strconv.FormatInt(quota, 10) + " 100000"
	}
	for file, val := range writes {
		if err := os.WriteFile(cg+"/"+file, []byte(val), 0o644); err != nil {
			return "some limits could not be applied live: " + err.Error()
		}
	}
	return ""
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
