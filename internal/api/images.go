package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/lxc"
	"github.com/games-on-whales/LXC2Docker/internal/oci"
	"github.com/games-on-whales/LXC2Docker/internal/store"
	"github.com/gorilla/mux"
)

// GET /images/json
func (h *Handler) listImages(w http.ResponseWriter, r *http.Request) {
	filt := parseFilters(r)
	records := h.store.ListImages()
	lpf, err := parseListFilters(r.URL.Query().Get("filters"))
	if err != nil {
		errResponse(w, http.StatusBadRequest, "invalid filters: "+err.Error())
		return
	}
	parsedUntil, err := parsePruneUntil(lpf)
	if err != nil {
		errResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	usage := map[string]int{}
	for _, c := range h.store.ListContainers() {
		usage[normalizeImageRef(c.Image)]++
	}

	wantDangling := danglingWant(filt["dangling"])

	grouped := map[string]*ImageSummary{}
	ids := []string{}
	for _, rec := range records {
		if !filt.matchImageReference(rec.Ref) {
			continue
		}
		if parsedUntil != nil && rec.Created.After(*parsedUntil) {
			continue
		}
		if !filt.matchLabel(rec.OCILabels) {
			continue
		}
		if wantDangling != nil && *wantDangling != imageIsDangling(rec) {
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
			Size:        imageSize(h.mgr.LXCPath(), h.mgr.PVEStorage(), rec),
			VirtualSize: imageSize(h.mgr.LXCPath(), h.mgr.PVEStorage(), rec),
			Labels:      labels,
			Containers:  usage[rec.Ref],
		}
		ids = append(ids, key)
	}
	out := make([]ImageSummary, 0, len(ids))
	for _, id := range ids {
		s := grouped[id]
		// Docker reports RepoTags/RepoDigests as sorted sets; dedupe so a
		// same-repo `docker tag` doesn't surface a repo@digest twice.
		s.RepoTags = sortedUnique(s.RepoTags)
		s.RepoDigests = sortedUnique(s.RepoDigests)
		out = append(out, *s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Created > out[j].Created
	})
	jsonResponse(w, http.StatusOK, out)
}

// imageRefsForID returns every repo tag — and their "<repo>@<digest>" refs —
// that resolve to the same image ID. The store keys images by ref, but
// `docker tag` copies the record under a new ref keeping the ID, and distro
// refs like ubuntu:22.04 / ubuntu:jammy share an ID, so several refs can point
// at one image. Docker's inspect reports every tag/digest pointing at the
// image (matching the aggregation /images/json already does), not just the
// queried ref. Results are sorted for deterministic output.
func (h *Handler) imageRefsForID(id string) (tags, digests []string) {
	tags = []string{}
	digests = []string{}
	for _, rec := range h.store.ListImages() {
		if rec.ID != id {
			continue
		}
		tags = append(tags, rec.Ref)
		digests = append(digests, digestRefs(rec)...)
	}
	return sortedUnique(tags), sortedUnique(digests)
}

// sortedUnique returns a new slice with the input sorted and de-duplicated.
// Docker reports RepoTags/RepoDigests as sets: aggregating across records that
// share an image ID (e.g. `docker tag nginx:latest nginx:stable` yields two
// records with the same repo@digest) must not emit a value twice. Always
// returns a non-nil slice so it marshals to [] rather than null.
func sortedUnique(in []string) []string {
	out := []string{}
	if len(in) == 0 {
		return out
	}
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	for i, s := range cp {
		if i == 0 || s != cp[i-1] {
			out = append(out, s)
		}
	}
	return out
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
	// A digest ref needs a repo; a dangling/untagged record (empty ref) would
	// otherwise yield a malformed "@sha256:...". Docker only lists repo@digest
	// for images that actually have a repository.
	if bare == "" {
		return []string{}
	}
	return []string{bare + "@" + rec.RepoDigest}
}

// imageSize returns the on-disk size of an image template's rootfs. For
// legacy LXC templates it walks the rootfs; for Proxmox CT templates it
// asks ZFS for the dataset's `used` property so the /images/json response
// stays fast even on large ZFS pools.
func imageSize(lxcPath, pveStorage string, rec *store.ImageRecord) int64 {
	if rec.TemplateVMID > 0 {
		// PVE template — ask ZFS for the template dataset's `used` size. The
		// daemon forms CT datasets as <pveStorage>/basevol-<vmid>-disk-0
		// (see Manager.RootfsPath), so the configured storage name is the ZFS
		// pool. Fall back to 0 when ZFS/the dataset can't be found.
		return zfsDatasetSize(pveStorage, rec)
	}
	if rec.TemplateName == "" {
		return 0
	}
	return rootfsSize(filepath.Join(lxcPath, rec.TemplateName, "rootfs"))
}

// zfsCandidatePools returns the ZFS pool names to probe for a template dataset,
// putting the daemon's configured storage first (the daemon uses the storage
// name as the pool name when forming datasets) and then a few common fallbacks
// for robustness. The configured pool is never duplicated.
func zfsCandidatePools(pveStorage string) []string {
	pools := make([]string, 0, 4)
	if pveStorage != "" {
		pools = append(pools, pveStorage)
	}
	for _, p := range []string{"large", "rpool", "tank"} {
		if p != pveStorage {
			pools = append(pools, p)
		}
	}
	return pools
}

// zfsDatasetSize runs `zfs get used` on the template dataset to pull its size.
// It probes the daemon's configured storage pool first (see zfsCandidatePools).
// If ZFS isn't present or the dataset can't be found, returns 0.
func zfsDatasetSize(pveStorage string, rec *store.ImageRecord) int64 {
	for _, pool := range zfsCandidatePools(pveStorage) {
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
	fromImage := strings.TrimSpace(r.URL.Query().Get("fromImage"))
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		tag = "latest"
	}
	if fromImage == "" {
		errResponse(w, http.StatusBadRequest, "fromImage query parameter is required")
		return
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

	alreadyPresent := h.store.GetImage(ref) != nil

	err := h.mgr.PullImageWith(ref, "amd64", lxc.PullOpts{
		Credentials: creds,
		OnStatus:    sendStatus,
		OnEvent:     sendEvent,
	})
	if err == nil {
		h.emitImage("pull", ref)
	}
	if err != nil {
		enc.Encode(map[string]any{
			"error": err.Error(),
			"errorDetail": map[string]string{
				"message": err.Error(),
			},
		})
		flush()
		return
	}

	// Docker emits a "Digest: sha256:..." line before the final status so the
	// client (and `docker pull` output) can report the content digest it
	// resolved. We record the registry manifest digest at pull time.
	if rec := h.store.GetImage(ref); rec != nil && rec.RepoDigest != "" {
		sendStatus("Digest: " + rec.RepoDigest)
	}

	if alreadyPresent {
		sendStatus(fmt.Sprintf("Status: Image is up to date for %s", ref))
	} else {
		sendStatus(fmt.Sprintf("Status: Downloaded newer image for %s", ref))
	}
}

// GET /images/search
func (h *Handler) searchImages(w http.ResponseWriter, r *http.Request) {
	rawTerm := strings.TrimSpace(r.URL.Query().Get("term"))
	term := strings.ToLower(rawTerm)
	if term == "" {
		errResponse(w, http.StatusBadRequest, "term query parameter is required")
		return
	}

	limit := 25
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			errResponse(w, http.StatusBadRequest, "invalid limit parameter")
			return
		}
		limit = n
	}

	seen := map[string]ImageSearchResult{}
	for _, rec := range h.store.ListImages() {
		name := shortenImageRef(normalizeImageRef(rec.Ref))
		cname := strings.ToLower(name)
		if !strings.Contains(cname, term) {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = ImageSearchResult{
			Name:        name,
			Description: "",
			StarCount:   0,
			IsOfficial:  false,
			IsAutomated: false,
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	results := make([]ImageSearchResult, 0, len(names))
	for _, name := range names {
		if limit == 0 {
			break
		}
		results = append(results, seen[name])
		if limit > 0 {
			limit--
		}
	}
	if len(results) == 0 {
		if synthetic := syntheticImageSearchName(rawTerm); synthetic != "" {
			results = append(results, ImageSearchResult{
				Name:        synthetic,
				Description: "Pullable image reference",
				StarCount:   0,
				IsOfficial:  false,
				IsAutomated: false,
			})
		}
	}

	jsonResponse(w, http.StatusOK, results)
}

func syntheticImageSearchName(term string) string {
	name := shortenImageRef(strings.TrimSpace(strings.ToLower(term)))
	if name == "" {
		return ""
	}
	if digest := strings.IndexByte(name, '@'); digest >= 0 {
		name = name[:digest]
	}
	lastSlash := strings.LastIndexByte(name, '/')
	lastColon := strings.LastIndexByte(name, ':')
	if lastColon > lastSlash {
		name = name[:lastColon]
	}
	return strings.TrimSpace(name)
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
	name := imageNameFromRequest(r)
	rec := h.store.GetImage(normalizeImageRef(name))
	if rec == nil {
		rec = h.findImageByID(name)
	}
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
	cfg := imageConfigFromRecord(rec)

	repoTags, repoDigests := h.imageRefsForID(rec.ID)

	resp := ImageInspect{
		ID:              "sha256:" + rec.ID,
		RepoTags:        repoTags,
		RepoDigests:     repoDigests,
		Comment:         rec.OCIComment,
		Created:         rec.Created.Format(time.RFC3339),
		Container:       rec.OCIContainer,
		Architecture:    rec.Arch,
		Variant:         rec.OCIVariant,
		Os:              "linux",
		OsVersion:       rec.Release,
		Size:            imageSize(h.mgr.LXCPath(), h.mgr.PVEStorage(), rec),
		VirtualSize:     imageSize(h.mgr.LXCPath(), h.mgr.PVEStorage(), rec),
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
		Author:        orDefault(rec.OCIAuthor, "docker-lxc-daemon"),
		DockerVersion: orDefault(rec.OCIDockerVersion, "24.0.0-lxc"),
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

func imageNameFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	if name := mux.Vars(r)["name"]; name != "" {
		return name
	}
	path := strings.Trim(strings.TrimSpace(r.URL.Path), "/")
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	for idx := 0; idx < len(parts); idx++ {
		if parts[idx] != "images" || idx+2 >= len(parts) || parts[len(parts)-1] != "json" {
			continue
		}
		return strings.Join(parts[idx+1:len(parts)-1], "/")
	}
	return ""
}

func imageConfigFromRecord(rec *store.ImageRecord) *ContainerConfig {
	if rec == nil {
		return normalizeContainerConfig(&ContainerConfig{})
	}
	volumes := map[string]struct{}{}
	for _, v := range rec.OCIVolumes {
		if v != "" {
			volumes[v] = struct{}{}
		}
	}
	exposed := map[string]struct{}{}
	for _, p := range rec.OCIPorts {
		if p != "" {
			exposed[p] = struct{}{}
		}
	}
	return normalizeContainerConfig(&ContainerConfig{
		Hostname:        rec.OCIHostname,
		Domainname:      rec.OCIDomainname,
		MacAddress:      rec.OCIMacAddress,
		User:            rec.OCIUser,
		AttachStdin:     rec.OCIAttachStdin,
		AttachStdout:    rec.OCIAttachStdout,
		AttachStderr:    rec.OCIAttachStderr,
		ExposedPorts:    exposed,
		Tty:             rec.OCITty,
		OpenStdin:       rec.OCIOpenStdin,
		StdinOnce:       rec.OCIStdinOnce,
		NetworkDisabled: rec.OCINetworkDisabled,
		ArgsEscaped:     rec.OCIArgsEscaped,
		Volumes:         volumes,
		Cmd:             rec.OCICmd,
		Entrypoint:      rec.OCIEntrypoint,
		Env:             rec.OCIEnv,
		Labels:          ensureMap(rec.OCILabels),
		WorkingDir:      rec.OCIWorkingDir,
		OnBuild:         append([]string{}, rec.OCIOnBuild...),
		Shell:           append([]string{}, rec.OCIShell...),
		StopSignal:      rec.OCIStopSignal,
		StopTimeout:     stopTimeoutPtr(rec.OCIStopTimeout),
		Healthcheck:     healthcheckFromImage(rec),
	})
}

func healthcheckFromImage(rec *store.ImageRecord) *Healthcheck {
	if rec == nil || rec.OCIHealthcheck == nil {
		return nil
	}
	hc := rec.OCIHealthcheck
	return &Healthcheck{
		Test:        append([]string{}, hc.Test...),
		Interval:    hc.Interval,
		Timeout:     hc.Timeout,
		Retries:     hc.Retries,
		StartPeriod: hc.StartPeriod,
	}
}

// DELETE /images/{name}  (docker rmi)
func (h *Handler) removeImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	ref := normalizeImageRef(name)
	force := boolValue(r, "force")
	if h.store.GetImage(ref) == nil {
		if byID := h.findImageByID(name); byID != nil {
			ref = byID.Ref
		} else {
			if force {
				jsonResponse(w, http.StatusOK, []map[string]string{})
				return
			}
			errResponse(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", name))
			return
		}
	}
	img := h.store.GetImage(ref)

	// `docker tag` copies the store record (sharing the same backing tarball/
	// dataset/template), so an image ID can have several refs. Removing one of
	// several refs is an UNTAG: drop just this ref's record and leave the
	// backing for the other tags. Manager.RemoveImage always destroys the
	// shared backing, so it must only run for the LAST ref.
	if img != nil {
		if refs, _ := h.imageRefsForID(img.ID); len(refs) > 1 {
			if err := h.store.RemoveImage(ref); err != nil {
				errResponse(w, http.StatusInternalServerError, err.Error())
				return
			}
			h.emitImage("untag", ref)
			jsonResponse(w, http.StatusOK, []map[string]string{{"Untagged": ref}})
			return
		}
	}

	// Last ref → the image itself is being deleted. Docker refuses (409) when a
	// container still references it, unless forced.
	if !force {
		if cid := h.imageInUse(img); cid != "" {
			errResponse(w, http.StatusConflict, fmt.Sprintf(
				"conflict: unable to remove repository reference %q (must force) - container %s is using its referenced image %s",
				name, shortID(cid), shortID(imageIDOf(img))))
			return
		}
	}

	if err := h.mgr.RemoveImage(ref); err != nil {
		if force {
			jsonResponse(w, http.StatusOK, []map[string]string{})
			return
		}
		// The in-use conflict was handled above with a 409; a RemoveImage
		// failure here is an unexpected backend error, which Docker reports as
		// 500 (409 is reserved for the conflict case).
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.emitImage("delete", ref)
	out := []map[string]string{{"Untagged": ref}}
	if img != nil {
		out = append(out, map[string]string{"Deleted": "sha256:" + img.ID})
	}
	jsonResponse(w, http.StatusOK, out)
}

// imageInUse returns the ID of a container that references the given image (by
// any of the image's refs or its recorded image ID), or "" if none. Used to
// gate `docker rmi` of the last ref with a 409, matching Docker.
func (h *Handler) imageInUse(img *store.ImageRecord) string {
	if img == nil {
		return ""
	}
	refs, _ := h.imageRefsForID(img.ID)
	want := map[string]bool{}
	for _, rr := range refs {
		want[normalizeImageRef(rr)] = true
	}
	for _, c := range h.store.ListContainers() {
		if want[normalizeImageRef(c.Image)] {
			return c.ID
		}
		if c.ImageID != "" && want[normalizeImageRef(c.ImageID)] {
			return c.ID
		}
	}
	return ""
}

// imageIDOf returns the store ID of img, or "" if nil.
func imageIDOf(img *store.ImageRecord) string {
	if img == nil {
		return ""
	}
	return img.ID
}

// shortID truncates an identifier to Docker's 12-character short form.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func danglingWant(vals []string) *bool {
	for _, v := range vals {
		switch v {
		case "1", "true":
			t := true
			return &t
		case "0", "false":
			f := false
			return &f
		}
	}
	return nil
}

func imageIsDangling(rec *store.ImageRecord) bool {
	return rec.Ref == "" || strings.HasSuffix(rec.Ref, "<none>:<none>")
}

func (h *Handler) findImageByID(id string) *store.ImageRecord {
	id = strings.TrimPrefix(id, "sha256:")
	if id == "" {
		return nil
	}
	for _, rec := range h.store.ListImages() {
		if rec.ID == id {
			return rec
		}
		if len(id) >= 4 && strings.HasPrefix(rec.ID, id) {
			return rec
		}
	}
	return nil
}

func normalizeImageRef(name string) string {
	if !strings.Contains(name, ":") {
		return name + ":latest"
	}
	return name
}
