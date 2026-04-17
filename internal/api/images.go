package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// GET /images/json
func (h *Handler) listImages(w http.ResponseWriter, r *http.Request) {
	records := h.store.ListImages()
	out := make([]ImageSummary, 0, len(records))
	for _, rec := range records {
		out = append(out, ImageSummary{
			ID:       "sha256:" + rec.ID,
			RepoTags: []string{rec.Ref},
			Created:  rec.Created.Unix(),
			Labels:   map[string]string{},
		})
	}
	jsonResponse(w, http.StatusOK, out)
}

// POST /images/create  (docker pull)
// Query params: fromImage=<name>, tag=<tag>
func (h *Handler) pullImage(w http.ResponseWriter, r *http.Request) {
	fromImage := r.URL.Query().Get("fromImage")
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		tag = "latest"
	}

	ref := fromImage
	if !strings.Contains(fromImage, ":") {
		ref = fromImage + ":" + tag
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	send := func(status string) {
		enc.Encode(map[string]string{"status": status})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	send(fmt.Sprintf("Pulling from %s", fromImage))

	err := h.mgr.PullImage(ref, "amd64", func(msg string) {
		send(msg)
	})
	if err != nil {
		send(fmt.Sprintf("Error: %s", err))
		return
	}

	send(fmt.Sprintf("Status: Downloaded newer image for %s", ref))
	h.publishEvent("image", "pull", ref, map[string]string{"name": ref})
}

// GET /images/{name}/json  (docker image inspect)
func (h *Handler) inspectImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	rec := h.store.GetImage(normalizeImageRef(name))
	if rec == nil {
		errResponse(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", name))
		return
	}
	jsonResponse(w, http.StatusOK, ImageInspect{
		ID:           "sha256:" + rec.ID,
		RepoTags:     []string{rec.Ref},
		Created:      rec.Created.Format(time.RFC3339),
		Architecture: rec.Arch,
		Os:           "linux",
		Labels:       map[string]string{},
	})
}

// DELETE /images/{name}  (docker rmi)
func (h *Handler) removeImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	ref := normalizeImageRef(name)
	if err := h.mgr.RemoveImage(ref); err != nil {
		errResponse(w, http.StatusConflict, err.Error())
		return
	}
	h.publishEvent("image", "delete", ref, map[string]string{"name": ref})
	jsonResponse(w, http.StatusOK, []map[string]string{
		{"Untagged": ref},
	})
}

func (h *Handler) imageHistory(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	rec := h.store.GetImage(normalizeImageRef(name))
	if rec == nil {
		errResponse(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", name))
		return
	}
	jsonResponse(w, http.StatusOK, []ImageHistoryItem{{
		ID:        "sha256:" + rec.ID,
		Created:   rec.Created.Unix(),
		CreatedBy: "docker-lxc-daemon import",
		Tags:      []string{rec.Ref},
		Comment:   "Imported into LXC template store",
	}})
}

func (h *Handler) tagImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	src := h.store.GetImage(normalizeImageRef(name))
	if src == nil {
		errResponse(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", name))
		return
	}
	repo := r.URL.Query().Get("repo")
	tag := r.URL.Query().Get("tag")
	if repo == "" {
		errResponse(w, http.StatusBadRequest, "repo is required")
		return
	}
	dstRef := repo
	if tag != "" && !strings.Contains(repo, ":") {
		dstRef += ":" + tag
	}
	dup := *src
	dup.Ref = normalizeImageRef(dstRef)
	if err := h.store.AddImage(&dup); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.publishEvent("image", "tag", dup.Ref, map[string]string{"name": dup.Ref})
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) searchImages(w http.ResponseWriter, r *http.Request) {
	term := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("term")))
	results := []ImageSearchResult{}
	for _, candidate := range []ImageSearchResult{
		{Name: "ubuntu", Description: "Ubuntu LXC base image", IsOfficial: true},
		{Name: "debian", Description: "Debian LXC base image", IsOfficial: true},
		{Name: "alpine", Description: "Alpine LXC base image", IsOfficial: true},
	} {
		if term == "" || strings.Contains(candidate.Name, term) || strings.Contains(strings.ToLower(candidate.Description), term) {
			results = append(results, candidate)
		}
	}
	jsonResponse(w, http.StatusOK, results)
}

func (h *Handler) pushImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	ref := normalizeImageRef(name)
	if h.store.GetImage(ref) == nil {
		errResponse(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", name))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	send := func(v any) {
		_ = enc.Encode(v)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	send(map[string]string{"status": "The push refers to repository [" + ref + "]"})
	send(map[string]any{
		"error":       "image push is not supported",
		"errorDetail": map[string]string{"message": "image push is not supported"},
	})
}

func normalizeImageRef(name string) string {
	if !strings.Contains(name, ":") {
		return name + ":latest"
	}
	return name
}
