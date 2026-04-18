package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
)

// GET /images/json
func (h *Handler) listImages(w http.ResponseWriter, r *http.Request) {
	filt := parseFilters(r)
	records := h.store.ListImages()

	out := make([]ImageSummary, 0, len(records))
	for _, rec := range records {
		if !filt.matchImageReference(rec.Ref) {
			continue
		}
		out = append(out, ImageSummary{
			ID:          "sha256:" + rec.ID,
			RepoTags:    []string{rec.Ref},
			RepoDigests: []string{},
			Created:     rec.Created.Unix(),
			Size:        imageSize(h.mgr.LXCPath(), rec),
			VirtualSize: imageSize(h.mgr.LXCPath(), rec),
			Labels:      map[string]string{},
			Containers:  -1, // Docker convention for "not computed"
		})
	}
	jsonResponse(w, http.StatusOK, out)
}

// imageSize returns the on-disk size of an image template's rootfs. Computing
// this per image can be slow on large ZFS datasets, so we best-effort via
// Walk and return 0 on errors rather than holding the list endpoint open.
func imageSize(lxcPath string, rec *store.ImageRecord) int64 {
	// For PVE templates the rootfs lives on a ZFS dataset we don't want to
	// traverse on every /images/json poll — return 0 rather than stall.
	if rec.TemplateVMID > 0 {
		return 0
	}
	if rec.TemplateName == "" {
		return 0
	}
	rootfs := filepath.Join(lxcPath, rec.TemplateName, "rootfs")
	var total int64
	filepath.WalkDir(rootfs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
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
}

// GET /images/{name}/json  (docker image inspect)
//
// Same handler services HEAD /images/{name}/json — Portainer's "is this
// image present?" check. We skip body writes when the request is HEAD but
// otherwise return the identical payload.
func (h *Handler) inspectImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	rec := h.store.GetImage(normalizeImageRef(name))
	if rec == nil {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		errResponse(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", name))
		return
	}
	// The embedded Config mirrors the OCI image config so Portainer's
	// "Duplicate" and "Run from image" modals pre-populate with the correct
	// entrypoint/cmd/env. Distro and App images don't have OCI configs; we
	// emit an empty Config so the shape is still correct.
	cfg := &ContainerConfig{
		Env:        rec.OCIEnv,
		Cmd:        rec.OCICmd,
		Entrypoint: rec.OCIEntrypoint,
		WorkingDir: rec.OCIWorkingDir,
		Labels:     map[string]string{},
	}
	if cfg.Env == nil {
		cfg.Env = []string{}
	}

	resp := ImageInspect{
		ID:              "sha256:" + rec.ID,
		RepoTags:        []string{rec.Ref},
		RepoDigests:     []string{},
		Created:         rec.Created.Format(time.RFC3339),
		Architecture:    rec.Arch,
		Os:              "linux",
		Size:            imageSize(h.mgr.LXCPath(), rec),
		VirtualSize:     imageSize(h.mgr.LXCPath(), rec),
		Config:          cfg,
		ContainerConfig: cfg,
		GraphDriver: GraphDriver{
			Name: "lxc",
			Data: map[string]string{},
		},
		RootFS: ImageRootFS{
			Type:   "layers",
			Layers: []string{"sha256:" + rec.ID},
		},
		Metadata: ImageMetadata{
			LastTagTime: rec.Created.Format(time.RFC3339),
		},
		Labels:        map[string]string{},
		Author:        "docker-lxc-daemon",
		DockerVersion: "24.0.0-lxc",
	}

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return
	}
	jsonResponse(w, http.StatusOK, resp)
}

// DELETE /images/{name}  (docker rmi)
func (h *Handler) removeImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	ref := normalizeImageRef(name)
	if err := h.mgr.RemoveImage(ref); err != nil {
		errResponse(w, http.StatusConflict, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, []map[string]string{
		{"Untagged": ref},
	})
}

func normalizeImageRef(name string) string {
	if !strings.Contains(name, ":") {
		return name + ":latest"
	}
	return name
}
