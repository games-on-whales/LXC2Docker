package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/oci"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
)

// GET /images/json
func (h *Handler) listImages(w http.ResponseWriter, r *http.Request) {
	filters, err := parseListFilters(r.URL.Query().Get("filters"))
	if err != nil {
		errResponse(w, http.StatusBadRequest, "invalid filters: "+err.Error())
		return
	}
	// Docker's `dangling` image filter is the odd one out: images without
	// tags. We never produce untagged images, so `dangling=true` yields an
	// empty list and `dangling=false` is a no-op.
	if vals := filters["dangling"]; len(vals) > 0 {
		onlyDangling := false
		for _, v := range vals {
			if v == "true" || v == "1" {
				onlyDangling = true
			}
		}
		if onlyDangling {
			jsonResponse(w, http.StatusOK, []ImageSummary{})
			return
		}
	}

	records := h.store.ListImages()
	out := make([]ImageSummary, 0, len(records))
	for _, rec := range records {
		if !matchesImageFilters(rec, filters) {
			continue
		}
		size := h.imageSize(rec)
		out = append(out, ImageSummary{
			ID:          "sha256:" + rec.ID,
			RepoTags:    []string{rec.Ref},
			Created:     rec.Created.Unix(),
			Size:        size,
			VirtualSize: size,
			Labels:      copyLabels(rec.OCILabels),
		})
	}
	jsonResponse(w, http.StatusOK, out)
}

func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// matchesImageFilters applies Docker's /images/json filter keys we support
// (reference, label). `reference` does a substring/prefix match on the tag
// — Docker's globbing support is richer, but Portainer only uses exact and
// prefix matches today.
func matchesImageFilters(rec *store.ImageRecord, f listFilters) bool {
	if vals := f["reference"]; len(vals) > 0 {
		ref := normalizeImageRef(rec.Ref)
		matched := false
		for _, v := range vals {
			v = strings.TrimRight(v, "*")
			if v == "" {
				matched = true
				break
			}
			if strings.Contains(ref, v) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if !matchesLabelFilter(f["label"], rec.OCILabels) {
		return false
	}
	return true
}

// POST /images/create  (docker pull, or docker import when fromSrc is set)
// Query params: fromImage=<name>, tag=<tag>, fromSrc=<url-or-dash>
func (h *Handler) pullImage(w http.ResponseWriter, r *http.Request) {
	fromImage := r.URL.Query().Get("fromImage")
	fromSrc := r.URL.Query().Get("fromSrc")
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		tag = "latest"
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

	// Portainer's "Import image" UI calls /images/create with fromSrc
	// instead of fromImage. We don't ingest rootfs tarballs into the LXC
	// template store yet, but returning a framed JSON error (with the
	// same envelope pull uses) keeps Portainer's progress dialog from
	// hanging on an empty stream.
	if fromSrc != "" && fromImage == "" {
		msg := "image import via fromSrc is not supported by docker-lxc-daemon; pull the image from a registry instead"
		send(map[string]any{
			"error":       msg,
			"errorDetail": map[string]string{"message": msg},
		})
		return
	}

	ref := fromImage
	if !strings.Contains(fromImage, ":") {
		ref = fromImage + ":" + tag
	}

	send(map[string]string{"status": fmt.Sprintf("Pulling from %s", fromImage)})

	if recovered, err := h.recoverImageRecord(ref); err != nil {
		send(map[string]any{
			"error":       err.Error(),
			"errorDetail": map[string]string{"message": err.Error()},
		})
		return
	} else if recovered {
		send(map[string]string{"status": fmt.Sprintf("Status: Image is up to date for %s", ref)})
		return
	}

	err := h.mgr.PullImage(ref, "amd64", func(msg string) {
		send(map[string]string{"status": msg})
	})
	if err != nil {
		send(map[string]any{
			"error":       err.Error(),
			"errorDetail": map[string]string{"message": err.Error()},
		})
		return
	}

	send(map[string]string{"status": fmt.Sprintf("Status: Downloaded newer image for %s", ref)})
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
	size := h.imageSize(rec)
	id := "sha256:" + rec.ID
	config := imageConfigFromRecord(rec)
	jsonResponse(w, http.StatusOK, ImageInspect{
		ID:              id,
		RepoTags:        []string{rec.Ref},
		RepoDigests:     []string{},
		Created:         rec.Created.Format(time.RFC3339),
		Architecture:    rec.Arch,
		Os:              "linux",
		Size:            size,
		VirtualSize:     size,
		Labels:          copyLabels(rec.OCILabels),
		DockerVersion:   "24.0.0",
		Author:          "docker-lxc-daemon",
		Config:          config,
		ContainerConfig: config,
		RootFS: ImageRootFS{
			Type:   "layers",
			Layers: []string{id},
		},
		GraphDriver: ImageGraphDriver{
			Name: "lxc",
			Data: map[string]string{},
		},
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
	size := h.imageSize(rec)
	history := synthesizeImageHistory(rec, size)
	jsonResponse(w, http.StatusOK, history)
}

// synthesizeImageHistory reconstructs a Dockerfile-style history list from
// the OCI config we captured at pull time. Portainer's image "History" tab
// shows the CreatedBy column, so one row per OCI directive (ENV/WORKDIR/
// EXPOSE/ENTRYPOINT/CMD) is much more informative than a single "import"
// row. The top entry carries the size; intermediate entries are marked 0.
func synthesizeImageHistory(rec *store.ImageRecord, size int64) []ImageHistoryItem {
	created := rec.Created.Unix()
	id := "sha256:" + rec.ID
	tags := []string{rec.Ref}
	items := []ImageHistoryItem{{
		ID:        id,
		Created:   created,
		CreatedBy: "docker-lxc-daemon import",
		Tags:      tags,
		Size:      size,
		Comment:   "Imported into LXC template store",
	}}
	for _, env := range rec.OCIEnv {
		items = append(items, ImageHistoryItem{
			ID:        "<missing>",
			Created:   created,
			CreatedBy: "/bin/sh -c #(nop)  ENV " + env,
		})
	}
	if rec.OCIWorkingDir != "" {
		items = append(items, ImageHistoryItem{
			ID:        "<missing>",
			Created:   created,
			CreatedBy: "/bin/sh -c #(nop) WORKDIR " + rec.OCIWorkingDir,
		})
	}
	for _, port := range rec.OCIPorts {
		items = append(items, ImageHistoryItem{
			ID:        "<missing>",
			Created:   created,
			CreatedBy: "/bin/sh -c #(nop)  EXPOSE " + port,
		})
	}
	if len(rec.OCIEntrypoint) > 0 {
		items = append(items, ImageHistoryItem{
			ID:        "<missing>",
			Created:   created,
			CreatedBy: "/bin/sh -c #(nop)  ENTRYPOINT " + dockerJSONList(rec.OCIEntrypoint),
		})
	}
	if len(rec.OCICmd) > 0 {
		items = append(items, ImageHistoryItem{
			ID:        "<missing>",
			Created:   created,
			CreatedBy: "/bin/sh -c #(nop)  CMD " + dockerJSONList(rec.OCICmd),
		})
	}
	return items
}

// dockerJSONList renders a slice the way Docker's history column does:
// ["/bin/sh","-c","..."] with quoted members. Kept tiny and self-contained
// — the real encoding/json would add escaping we don't need here.
func dockerJSONList(xs []string) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, `"`+strings.ReplaceAll(x, `"`, `\"`)+`"`)
	}
	return "[" + strings.Join(parts, ",") + "]"
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
	index := map[string]ImageSearchResult{}
	for _, rec := range h.store.ListImages() {
		name := imageSearchName(rec.Ref)
		addImageSearchResult(index, ImageSearchResult{
			Name:        name,
			Description: "Image available locally in docker-lxc-daemon",
			IsOfficial:  strings.Count(name, "/") == 0,
		}, term)
	}
	for _, candidate := range curatedImageSearchResults() {
		addImageSearchResult(index, candidate, term)
	}
	results := make([]ImageSearchResult, 0, len(index))
	for _, candidate := range index {
		results = append(results, candidate)
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

// imageConfigFromRecord materialises an ImageConfig from the OCI metadata
// captured during pull. ExposedPorts is built from OCIPorts (strings like
// "80/tcp") using the map-of-empty-struct shape Docker returns.
func imageConfigFromRecord(rec *store.ImageRecord) *ImageConfig {
	exposed := map[string]struct{}{}
	for _, p := range rec.OCIPorts {
		if p != "" {
			exposed[p] = struct{}{}
		}
	}
	volumes := map[string]struct{}{}
	for _, v := range rec.OCIVolumes {
		if v != "" {
			volumes[v] = struct{}{}
		}
	}
	var volumesMap map[string]struct{}
	if len(volumes) > 0 {
		volumesMap = volumes
	}
	return &ImageConfig{
		Hostname:     "",
		Image:        rec.Ref,
		Env:          append([]string{}, rec.OCIEnv...),
		Cmd:          append([]string{}, rec.OCICmd...),
		Entrypoint:   append([]string{}, rec.OCIEntrypoint...),
		WorkingDir:   rec.OCIWorkingDir,
		Labels:       copyLabels(rec.OCILabels),
		ExposedPorts: exposed,
		Volumes:      volumesMap,
		User:         rec.OCIUser,
		StopSignal:   rec.OCIStopSignal,
		Healthcheck:  healthcheckFromRecord(rec.OCIHealthcheck),
	}
}

// imageSize returns a best-effort on-disk size for an image. ZFS-backed
// templates use `zfs get logicalreferenced` on the @tmpl snapshot; directory-
// backed templates fall back to walking the rootfs. On failure we return 0
// rather than an error because Portainer still renders the row usefully.
func (h *Handler) imageSize(rec *store.ImageRecord) int64 {
	if rec == nil {
		return 0
	}
	if rec.TemplateDataset != "" {
		if n, ok := zfsLogicalReferenced(rec.TemplateDataset); ok {
			return n
		}
	}
	if root := h.mgr.ImageRootfsPath(rec.Ref); root != "" {
		if n, err := dirSize(root); err == nil && n > 0 {
			return n
		}
	}
	// Legacy OCI records store only TemplateName even when their data lives
	// in ZFS. Mirror the recoverImageRecord logic and probe the guessed
	// dataset path so those rows don't render as 0 B forever.
	if storage := h.mgr.PVEStorage(); storage != "" {
		guess := fmt.Sprintf("%s/dld-templates/%s", storage, oci.SafeDirName(rec.Ref))
		if n, ok := zfsLogicalReferenced(guess); ok {
			return n
		}
	}
	return 0
}

func zfsLogicalReferenced(dataset string) (int64, bool) {
	out, err := exec.Command("zfs", "get", "-Hpo", "value",
		"logicalreferenced", dataset+"@tmpl").Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func normalizeImageRef(name string) string {
	if !strings.Contains(name, ":") {
		return name + ":latest"
	}
	return name
}

func addImageSearchResult(index map[string]ImageSearchResult, candidate ImageSearchResult, term string) {
	nameKey := strings.ToLower(candidate.Name)
	if term != "" {
		haystack := strings.ToLower(candidate.Name + " " + candidate.Description)
		if !strings.Contains(haystack, term) {
			return
		}
	}
	if existing, ok := index[nameKey]; ok {
		index[nameKey] = mergeImageSearchResult(existing, candidate)
		return
	}
	index[nameKey] = candidate
}

func mergeImageSearchResult(a, b ImageSearchResult) ImageSearchResult {
	out := a
	aLocal := strings.Contains(strings.ToLower(a.Description), "available locally")
	bLocal := strings.Contains(strings.ToLower(b.Description), "available locally")
	if b.StarCount > out.StarCount {
		out.StarCount = b.StarCount
	}
	out.IsOfficial = out.IsOfficial || b.IsOfficial
	out.IsAutomated = out.IsAutomated || b.IsAutomated
	if aLocal && !bLocal {
		out.Description = b.Description
	} else if !aLocal && bLocal {
		// Keep the richer non-local description already present.
	} else if len(b.Description) > len(out.Description) {
		out.Description = b.Description
	}
	if aLocal || bLocal {
		if !strings.Contains(strings.ToLower(out.Description), "available locally") {
			out.Description += "; available locally"
		}
	}
	return out
}

func imageSearchName(ref string) string {
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon]
	}
	return ref
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

func (h *Handler) recoverImageRecord(ref string) (bool, error) {
	if !h.mgr.UsePVE() {
		return h.store.GetImage(ref) != nil, nil
	}
	rec := h.store.GetImage(ref)
	if rec != nil && imageTemplateReady(h.mgr.PVEStorage(), rec) {
		return true, nil
	}
	dataset := fmt.Sprintf("%s/dld-templates/%s", h.mgr.PVEStorage(), oci.SafeDirName(ref))
	if exec.Command("zfs", "list", "-t", "snapshot", "-o", "name", "-H", dataset+"@tmpl").Run() != nil {
		return false, nil
	}
	if rec == nil {
		rec = &store.ImageRecord{
			ID:      "oci_" + oci.SafeDirName(ref),
			Ref:     ref,
			Arch:    "amd64",
			Created: time.Now(),
		}
	}
	rec.TemplateDataset = dataset
	if err := h.store.AddImage(rec); err != nil {
		return false, err
	}
	return true, nil
}

func imageTemplateReady(pveStorage string, rec *store.ImageRecord) bool {
	if rec == nil {
		return false
	}
	switch {
	case rec.TemplateDataset != "":
		return exec.Command("zfs", "list", "-t", "snapshot", "-o", "name", "-H", rec.TemplateDataset+"@tmpl").Run() == nil
	case rec.TemplateVMID > 0:
		return true
	case rec.TemplateName != "":
		return true
	default:
		if pveStorage == "" {
			return false
		}
		return false
	}
}
