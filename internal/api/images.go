package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
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

	usage := map[string]int{}
	for _, c := range h.store.ListContainers() {
		usage[normalizeImageRef(c.Image)]++
	}

	grouped := map[string]*ImageSummary{}
	ids := []string{}
	for _, rec := range records {
		if !filt.matchImageReference(rec.Ref) {
			continue
		}
		key := rec.ID
		if cur, ok := grouped[key]; ok {
			cur.RepoTags = append(cur.RepoTags, rec.Ref)
			for _, d := range digestRefs(rec) {
				cur.RepoDigests = append(cur.RepoDigests, d)
			}
			cur.Containers += usage[rec.Ref]
			continue
		}
		labels := rec.OCILabels
		if labels == nil {
			labels = map[string]string{}
		}
		grouped[key] = &ImageSummary{
			ID:          "sha256:" + rec.ID,
			RepoTags:    []string{rec.Ref},
			RepoDigests: digestRefs(rec),
			Created:     rec.Created.Unix(),
			Size:        imageSize(h.mgr.LXCPath(), rec),
			VirtualSize: imageSize(h.mgr.LXCPath(), rec),
			Labels:      labels,
			Containers:  usage[rec.Ref],
		}
		ids = append(ids, key)
	}
	out := make([]ImageSummary, 0, len(ids))
	for _, id := range ids {
		out = append(out, *grouped[id])
	}
	jsonResponse(w, http.StatusOK, out)
}

// digestRefs returns RepoDigests in Docker's "<repo>@<digest>" shape. If
// we captured a manifest digest at pull time (OCI pulls only), we emit one
// entry; otherwise the array is empty. Portainer's image detail page shows
// these under "Digests".
func digestRefs(rec *store.ImageRecord) []string {
	if rec.RepoDigest == "" {
		return []string{}
	}
	bare := rec.Ref
	if i := strings.Index(bare, ":"); i != -1 {
		bare = bare[:i]
	}
	return []string{bare + "@" + rec.RepoDigest}
}

// imageSize returns the on-disk size of an image template's rootfs. For
// legacy LXC templates it walks the rootfs; for Proxmox CT templates it
// asks ZFS for the dataset's `used` property so the /images/json response
// stays fast even on large ZFS pools.
func imageSize(lxcPath string, rec *store.ImageRecord) int64 {
	if rec.TemplateVMID > 0 {
		// PVE template — ask ZFS. Form: <pool>/basevol-<vmid>-disk-0.
		// We infer the pool from the rec's template vs the daemon's
		// configured storage; callers pass lxcPath but not storage. A
		// storage-name lookup would require threading the Manager through,
		// so we fall back to 0 when we can't determine the dataset.
		return zfsDatasetSize(rec)
	}
	if rec.TemplateName == "" {
		return 0
	}
	return rootfsSize(filepath.Join(lxcPath, rec.TemplateName, "rootfs"))
}

// zfsDatasetSize runs `zfs list -H -p -o used` to pull the template dataset
// size. The caller provides the image record; we derive the dataset name
// using the VMID and a small whitelist of common PVE storage names. If ZFS
// isn't present or the dataset can't be found, returns 0.
func zfsDatasetSize(rec *store.ImageRecord) int64 {
	// Without a direct reference to the daemon's pveStorage string we'd
	// have to grep for matching datasets; that's costly and unnecessary.
	// Try a simple `zfs get` against each known pool name.
	for _, pool := range []string{"large", "rpool", "tank"} {
		dataset := fmt.Sprintf("%s/basevol-%d-disk-0", pool, rec.TemplateVMID)
		out, err := exec.Command("zfs", "get", "-H", "-p", "-o", "value", "used", dataset).Output()
		if err == nil {
			n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			if err == nil {
				return n
			}
		}
	}
	return 0
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
		RepoDigests:     digestRefs(rec),
		Created:         rec.Created.Format(time.RFC3339),
		Architecture:    rec.Arch,
		Os:              "linux",
		OsVersion:       rec.Release,
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
		body, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		return
	}
	jsonResponse(w, http.StatusOK, resp)
}

// DELETE /images/{name}  (docker rmi)
func (h *Handler) removeImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	ref := normalizeImageRef(name)
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	if h.store.GetImage(ref) == nil {
		if force {
			jsonResponse(w, http.StatusOK, []map[string]string{})
			return
		}
		errResponse(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", name))
		return
	}
	if err := h.mgr.RemoveImage(ref); err != nil {
		if force {
			jsonResponse(w, http.StatusOK, []map[string]string{})
			return
		}
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
