package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
	results := make([]ImageSearchResult, 0, 16)
	seen := map[string]struct{}{}
	for _, rec := range h.store.ListImages() {
		name := strings.TrimSuffix(rec.Ref, ":latest")
		addImageSearchResult(results, seen, ImageSearchResult{
			Name:        name,
			Description: "Local image available in docker-lxc-daemon",
			IsOfficial:  strings.Count(name, "/") == 0,
		}, term, &results)
	}
	for _, candidate := range curatedImageSearchResults() {
		addImageSearchResult(results, seen, candidate, term, &results)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].StarCount != results[j].StarCount {
			return results[i].StarCount > results[j].StarCount
		}
		return results[i].Name < results[j].Name
	})
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

func addImageSearchResult(current []ImageSearchResult, seen map[string]struct{}, candidate ImageSearchResult, term string, dst *[]ImageSearchResult) {
	nameKey := strings.ToLower(candidate.Name)
	if _, ok := seen[nameKey]; ok {
		return
	}
	if term != "" {
		haystack := strings.ToLower(candidate.Name + " " + candidate.Description)
		if !strings.Contains(haystack, term) {
			return
		}
	}
	seen[nameKey] = struct{}{}
	*dst = append(*dst, candidate)
}

func curatedImageSearchResults() []ImageSearchResult {
	return []ImageSearchResult{
		{Name: "alpine", Description: "Minimal Alpine Linux base image", StarCount: 9000, IsOfficial: true},
		{Name: "ubuntu", Description: "Ubuntu base image", StarCount: 15000, IsOfficial: true},
		{Name: "debian", Description: "Debian base image", StarCount: 5000, IsOfficial: true},
		{Name: "nginx", Description: "Official build of Nginx", StarCount: 20000, IsOfficial: true},
		{Name: "redis", Description: "Official build of Redis", StarCount: 19000, IsOfficial: true},
		{Name: "postgres", Description: "Official PostgreSQL image", StarCount: 17000, IsOfficial: true},
		{Name: "mariadb", Description: "Official MariaDB server image", StarCount: 6000, IsOfficial: true},
		{Name: "mysql", Description: "Official MySQL server image", StarCount: 14000, IsOfficial: true},
		{Name: "busybox", Description: "BusyBox base image", StarCount: 3500, IsOfficial: true},
		{Name: "portainer/portainer-ce", Description: "Portainer Community Edition", StarCount: 3500, IsOfficial: false},
		{Name: "hello-world", Description: "Hello from Docker", StarCount: 3000, IsOfficial: true},
		{Name: "traefik", Description: "Cloud native edge router", StarCount: 12000, IsOfficial: true},
		{Name: "grafana/grafana", Description: "Grafana observability platform", StarCount: 4500, IsOfficial: false},
		{Name: "prom/prometheus", Description: "Prometheus monitoring server", StarCount: 2800, IsOfficial: false},
	}
}
