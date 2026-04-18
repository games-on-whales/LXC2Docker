package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/lxc"
	"github.com/games-on-whales/docker-lxc-daemon/internal/oci"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
)

// GET /images/json
func (h *Handler) listImages(w http.ResponseWriter, r *http.Request) {
	filt := parseFilters(r)
	records := h.store.ListImages()

	// Build a one-shot usage index so we emit a real Containers count per
	// image instead of -1. Portainer's Images tab uses this to enable the
	// "Remove unused" button and to block delete-with-usage.
	usage := map[string]int{}
	for _, c := range h.store.ListContainers() {
		usage[normalizeImageRef(c.Image)]++
	}

	out := make([]ImageSummary, 0, len(records))
	for _, rec := range records {
		if !filt.matchImageReference(rec.Ref) {
			continue
		}
		labels := rec.OCILabels
		if labels == nil {
			labels = map[string]string{}
		}
		out = append(out, ImageSummary{
			ID:          "sha256:" + rec.ID,
			RepoTags:    []string{rec.Ref},
			RepoDigests: []string{},
			Created:     rec.Created.Unix(),
			Size:        imageSize(h.mgr.LXCPath(), rec),
			VirtualSize: imageSize(h.mgr.LXCPath(), rec),
			Labels:      labels,
			Containers:  usage[rec.Ref],
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
// Headers: X-Registry-Auth — base64-encoded JSON credentials (Portainer
// sets this when the user has a registry configured for the image ref).
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

	creds := decodeRegistryAuth(r.Header.Get("X-Registry-Auth"))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	sendStatus := func(status string) {
		enc.Encode(map[string]string{"status": status})
		flush()
	}
	sendEvent := func(ev oci.ProgressEvent) {
		frame := map[string]any{
			"status": ev.Status,
		}
		if ev.ID != "" {
			frame["id"] = ev.ID
		}
		if ev.Total > 0 || ev.Current > 0 {
			frame["progressDetail"] = map[string]int64{
				"current": ev.Current,
				"total":   ev.Total,
			}
			// Docker also includes a human-readable progress string;
			// Portainer renders the bar from progressDetail regardless,
			// so we skip the redundant text.
		}
		enc.Encode(frame)
		flush()
	}

	sendStatus(fmt.Sprintf("Pulling from %s", fromImage))

	err := h.mgr.PullImageWith(ref, "amd64", lxc.PullOpts{
		Credentials: creds,
		OnStatus:    sendStatus,
		OnEvent:     sendEvent,
	})
	if err == nil {
		h.emitImage("pull", ref)
	}
	if err != nil {
		// Match Docker's error-frame shape — Portainer displays the
		// `errorDetail.message` field verbatim in the pull modal.
		enc.Encode(map[string]any{
			"error": err.Error(),
			"errorDetail": map[string]string{
				"message": err.Error(),
			},
		})
		flush()
		return
	}

	sendStatus(fmt.Sprintf("Status: Downloaded newer image for %s", ref))
}

// decodeRegistryAuth parses Docker's X-Registry-Auth header, a base64url JSON
// object. When the header is empty or malformed we return "" — skopeo then
// does an anonymous pull, which matches the behavior before credentials
// support was added.
//
// Docker's client sets the base64 with no padding; skopeo wants
// "username:password", so we collapse identitytoken to token form when
// that's the only credential present.
func decodeRegistryAuth(header string) string {
	if header == "" {
		return ""
	}
	// Docker uses URL-safe base64 without padding. The stdlib strict decoder
	// rejects both — try the permissive ones in order.
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(header); err != nil {
			if raw, err = base64.URLEncoding.DecodeString(header); err != nil {
				return ""
			}
		}
	}
	var cfg struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		Auth          string `json:"auth"` // base64("user:pass")
		IdentityToken string `json:"identitytoken"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	if cfg.Username != "" && cfg.Password != "" {
		return cfg.Username + ":" + cfg.Password
	}
	// `auth` is pre-encoded "user:pass"; skopeo accepts the decoded form.
	if cfg.Auth != "" {
		if dec, err := base64.StdEncoding.DecodeString(cfg.Auth); err == nil {
			return string(dec)
		}
	}
	if cfg.IdentityToken != "" {
		// Bearer tokens are passed to skopeo as "<oauth>:<token>" — most
		// OCI registries accept this shape. Callers using identity tokens
		// probably want to configure registries separately anyway.
		return "<token>:" + cfg.IdentityToken
	}
	return ""
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
	labels := rec.OCILabels
	if labels == nil {
		labels = map[string]string{}
	}
	cfg := &ContainerConfig{
		Env:        rec.OCIEnv,
		Cmd:        rec.OCICmd,
		Entrypoint: rec.OCIEntrypoint,
		WorkingDir: rec.OCIWorkingDir,
		Labels:     labels,
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
		Labels:        labels,
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
	h.emitImage("delete", ref)
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
