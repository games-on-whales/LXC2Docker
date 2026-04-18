package api

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/oci"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
)

// GET /images/{name}/get — Docker save single image.
func (h *Handler) saveImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	rec := h.store.GetImage(normalizeImageRef(name))
	if rec == nil {
		errResponse(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", name))
		return
	}
	h.writeSaveBundle(w, []*store.ImageRecord{rec})
}

// GET /images/get?names=a,b,c — Docker's multi-image save.
func (h *Handler) saveImages(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query()["names"]
	// Portainer's bulk save submits comma-separated values inside a single
	// `names` key; docker CLI sends a repeated `names=` query string.
	// Accept both shapes.
	var names []string
	for _, v := range raw {
		for _, n := range strings.Split(v, ",") {
			n = strings.TrimSpace(n)
			if n != "" {
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		errResponse(w, http.StatusBadRequest, "names query parameter is required")
		return
	}
	recs := make([]*store.ImageRecord, 0, len(names))
	for _, n := range names {
		rec := h.store.GetImage(normalizeImageRef(n))
		if rec == nil {
			errResponse(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", n))
			return
		}
		recs = append(recs, rec)
	}
	h.writeSaveBundle(w, recs)
}

// writeSaveBundle streams a Docker-save-format tar carrying one or more
// images. The emitted tar contains:
//   - <layerID>/layer.tar    — the rootfs as a flat tar (one dir per image)
//   - <configSHA>.json       — an OCI image config per image
//   - manifest.json          — one entry per image
//   - repositories           — legacy v1 tag map so older docker CLIs load it
func (h *Handler) writeSaveBundle(w http.ResponseWriter, recs []*store.ImageRecord) {
	stage, err := os.MkdirTemp("", "dld-save-*")
	if err != nil {
		errResponse(w, http.StatusInternalServerError, "stage: "+err.Error())
		return
	}
	defer os.RemoveAll(stage)

	type perImage struct {
		rec       *store.ImageRecord
		layerPath string
		layerDir  string
		layerSHA  string
		cfgBytes  []byte
		cfgSHA    string
	}
	images := make([]perImage, 0, len(recs))

	for _, rec := range recs {
		rootfs, cleanup, err := h.openImageRootfs(rec)
		if err != nil {
			errResponse(w, http.StatusInternalServerError, "resolve rootfs: "+err.Error())
			return
		}
		defer cleanup()

		layerDir := filepath.Join(stage, "layer-"+oci.SafeDirName(rec.Ref))
		if err := os.MkdirAll(layerDir, 0o755); err != nil {
			errResponse(w, http.StatusInternalServerError, "stage: "+err.Error())
			return
		}
		layerPath := filepath.Join(layerDir, "layer.tar")
		layerSHA, err := writeLayerTar(nil, rootfs, layerPath)
		if err != nil {
			errResponse(w, http.StatusInternalServerError, "tar rootfs: "+err.Error())
			return
		}
		cfgBytes, cfgSHA, err := synthesiseImageConfig(rec, layerSHA)
		if err != nil {
			errResponse(w, http.StatusInternalServerError, "synthesise config: "+err.Error())
			return
		}
		images = append(images, perImage{
			rec:       rec,
			layerPath: layerPath,
			layerDir:  filepath.Base(layerDir),
			layerSHA:  layerSHA,
			cfgBytes:  cfgBytes,
			cfgSHA:    cfgSHA,
		})
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	tw := tar.NewWriter(w)
	defer tw.Close()

	// manifest.json — one entry per image; layer paths prefixed by per-
	// image dir so bundles with duplicate layer.tar filenames don't clash.
	manifest := make([]map[string]any, 0, len(images))
	repositories := map[string]map[string]string{}
	for _, img := range images {
		manifest = append(manifest, map[string]any{
			"Config":   img.cfgSHA + ".json",
			"RepoTags": []string{img.rec.Ref},
			"Layers":   []string{img.layerDir + "/layer.tar"},
		})
		repo, tag := splitImageRef(img.rec.Ref)
		if repositories[repo] == nil {
			repositories[repo] = map[string]string{}
		}
		repositories[repo][tag] = img.cfgSHA
	}
	manifestJSON, _ := json.Marshal(manifest)
	if err := writeTarFile(tw, "manifest.json", manifestJSON, 0o644); err != nil {
		return
	}
	repositoriesJSON, _ := json.Marshal(repositories)
	if err := writeTarFile(tw, "repositories", repositoriesJSON, 0o644); err != nil {
		return
	}
	for _, img := range images {
		if err := writeTarFile(tw, img.cfgSHA+".json", img.cfgBytes, 0o644); err != nil {
			return
		}
		// Emit the per-image layer dir entry first, then the layer.tar,
		// so tar unpackers that require parent dirs don't trip up.
		if err := tw.WriteHeader(&tar.Header{
			Name:     img.layerDir + "/",
			Mode:     0o755,
			Typeflag: tar.TypeDir,
			ModTime:  time.Now(),
		}); err != nil {
			return
		}
		f, err := os.Open(img.layerPath)
		if err != nil {
			return
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:    img.layerDir + "/layer.tar",
			Size:    fi.Size(),
			Mode:    0o644,
			ModTime: time.Now(),
		}); err != nil {
			f.Close()
			return
		}
		_, _ = io.Copy(tw, f)
		f.Close()
	}
}

// openImageRootfs resolves an image's rootfs to a directory path and
// returns a cleanup hook the caller must invoke. For ZFS-backed templates
// we read straight from the @tmpl snapshot via ZFS's .zfs/snapshot
// accessor — no clone needed. For directory-backed templates the existing
// rootfs path is used directly. Both paths produce a no-op cleanup.
func (h *Handler) openImageRootfs(rec *store.ImageRecord) (string, func(), error) {
	noop := func() {}
	if path := h.mgr.ImageRootfsPath(rec.Ref); path != "" {
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			return path, noop, nil
		}
	}
	// Legacy OCI records that kept only TemplateName can still point at
	// a dataset — mirror recoverImageRecord's guess so they work too.
	if rec.TemplateDataset == "" {
		if storage := h.mgr.PVEStorage(); storage != "" {
			guess := fmt.Sprintf("%s/dld-templates/%s", storage, oci.SafeDirName(rec.Ref))
			if _, err := exec.Command("zfs", "list", "-H", "-o", "name", guess).Output(); err == nil {
				rec.TemplateDataset = guess
			}
		}
	}
	if rec.TemplateDataset == "" {
		return "", noop, fmt.Errorf("image %s has no resolvable rootfs", rec.Ref)
	}
	mp, err := zfsMountpoint(rec.TemplateDataset)
	if err != nil {
		return "", noop, fmt.Errorf("resolve template mountpoint: %w", err)
	}
	snapPath := filepath.Join(mp, ".zfs", "snapshot", "tmpl")
	if fi, err := os.Stat(snapPath); err != nil || !fi.IsDir() {
		return "", noop, fmt.Errorf("template %s has no @tmpl snapshot accessible at %s", rec.TemplateDataset, snapPath)
	}
	return snapPath, noop, nil
}

func zfsMountpoint(dataset string) (string, error) {
	out, err := exec.Command("zfs", "get", "-Hpo", "value", "mountpoint", dataset).Output()
	if err != nil {
		return "", err
	}
	mp := strings.TrimSpace(string(out))
	if mp == "" || mp == "-" || mp == "none" {
		return "", fmt.Errorf("dataset %s has no mountpoint", dataset)
	}
	return mp, nil
}

func splitImageRef(ref string) (string, string) {
	repo := strings.TrimSpace(ref)
	if repo == "" {
		return "", "latest"
	}
	if i := strings.Index(repo, ":"); i != -1 {
		return repo[:i], repo[i+1:]
	}
	return repo, "latest"
}

func copyLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// writeLayerTar tars root into outPath and returns the payload's sha256.
// Uses the host `tar` binary for speed and correct handling of xattrs,
// symlinks, and device files that pure-Go tar would mangle.
func writeLayerTar(ctx any, root, outPath string) (string, error) {
	out, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	defer out.Close()
	h := sha256.New()
	mw := io.MultiWriter(out, h)
	cmd := exec.Command("tar", "-cf", "-", "-C", root, ".")
	cmd.Stdout = mw
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return "", err
	}
	stderrBuf, _ := io.ReadAll(stderr)
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("tar: %w (%s)", err, strings.TrimSpace(string(stderrBuf)))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// synthesiseImageConfig materialises a Docker-compatible image config JSON
// from the OCI metadata stored on the image record plus the layer digest
// computed by writeLayerTar. Returns the JSON bytes and its sha256 (used as
// the filename in the outer tar).
func synthesiseImageConfig(rec *store.ImageRecord, layerSHA string) ([]byte, string, error) {
	cfg := map[string]any{
		"Env":        append([]string{}, rec.OCIEnv...),
		"Cmd":        append([]string{}, rec.OCICmd...),
		"Entrypoint": append([]string{}, rec.OCIEntrypoint...),
		"WorkingDir": rec.OCIWorkingDir,
		"User":       rec.OCIUser,
		"StopSignal": rec.OCIStopSignal,
		"Labels":     copyLabels(rec.OCILabels),
	}
	if ep := rec.OCIPorts; len(ep) > 0 {
		ports := map[string]struct{}{}
		for _, p := range ep {
			ports[p] = struct{}{}
		}
		cfg["ExposedPorts"] = ports
	}
	if hc := rec.OCIHealthcheck; hc != nil {
		cfg["Healthcheck"] = map[string]any{
			"Test":        hc.Test,
			"Interval":    hc.Interval,
			"Timeout":     hc.Timeout,
			"StartPeriod": hc.StartPeriod,
			"Retries":     hc.Retries,
		}
	}
	arch := rec.Arch
	if arch == "" {
		arch = "amd64"
	}
	img := map[string]any{
		"architecture": arch,
		"os":           "linux",
		"created":      rec.Created.UTC().Format(time.RFC3339Nano),
		"config":       cfg,
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{"sha256:" + layerSHA},
		},
		"history": []map[string]any{{
			"created":    rec.Created.UTC().Format(time.RFC3339Nano),
			"created_by": "docker-lxc-daemon save",
		}},
	}
	body, err := json.Marshal(img)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

// writeTarFile is a helper for adding a single in-memory file to an open
// archive/tar.Writer.
func writeTarFile(tw *tar.Writer, name string, body []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Size:    int64(len(body)),
		Mode:    mode,
		ModTime: time.Now(),
	}); err != nil {
		return err
	}
	_, err := tw.Write(body)
	return err
}

// saveManifestEntry mirrors the shape of one element in a docker-save
// manifest.json array.
type saveManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// saveImageConfig mirrors the subset of Docker's image config JSON we read
// back during load. Fields line up with the saveImage synthesis so a save →
// load round-trip preserves the OCI metadata we track.
type saveImageConfig struct {
	Architecture string `json:"architecture"`
	Config       struct {
		Env          []string            `json:"Env"`
		Cmd          []string            `json:"Cmd"`
		Entrypoint   []string            `json:"Entrypoint"`
		WorkingDir   string              `json:"WorkingDir"`
		User         string              `json:"User"`
		StopSignal   string              `json:"StopSignal"`
		Labels       map[string]string   `json:"Labels"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Healthcheck  *struct {
			Test        []string `json:"Test"`
			Interval    int64    `json:"Interval"`
			Timeout     int64    `json:"Timeout"`
			StartPeriod int64    `json:"StartPeriod"`
			Retries     int      `json:"Retries"`
		} `json:"Healthcheck"`
	} `json:"config"`
}

// POST /images/load — Docker load. Accepts a tar body produced by
// docker save (or /images/{name}/get) and registers the contained image
// as a new template.
//
// Portainer's "Upload image" button and docker load both call this. The
// response is a newline-delimited JSON stream so clients can tail
// progress messages.
func (h *Handler) loadImage(w http.ResponseWriter, r *http.Request) {
	if h.mgr.PVEStorage() == "" {
		errResponse(w, http.StatusNotImplemented,
			"image load requires a PVE storage pool; legacy directory mode is not supported yet")
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
	fail := func(msg string) {
		send(map[string]any{
			"error":       msg,
			"errorDetail": map[string]string{"message": msg},
		})
	}

	// Stage the uploaded tar to disk so we can read entries non-
	// sequentially — docker save emits manifest.json first but not in all
	// versions, so tolerate any order by materialising the whole bundle.
	stage, err := os.MkdirTemp("", "dld-load-*")
	if err != nil {
		fail("stage: " + err.Error())
		return
	}
	defer os.RemoveAll(stage)

	if err := extractBundleTar(r.Body, stage); err != nil {
		fail("unpack bundle: " + err.Error())
		return
	}

	manifestPath := filepath.Join(stage, "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		fail("missing manifest.json")
		return
	}
	var manifest []saveManifestEntry
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		fail("parse manifest: " + err.Error())
		return
	}
	if len(manifest) == 0 {
		fail("manifest is empty")
		return
	}

	for _, entry := range manifest {
		if err := h.importLoadedImage(stage, entry, send); err != nil {
			fail(err.Error())
			return
		}
	}
}

// importLoadedImage imports a single manifest entry: extracts its layer
// into a fresh ZFS dataset, snapshots @tmpl, and persists an ImageRecord
// carrying the OCI metadata from the config JSON.
func (h *Handler) importLoadedImage(stage string, entry saveManifestEntry, send func(any)) error {
	if len(entry.Layers) == 0 {
		return fmt.Errorf("manifest entry has no layers")
	}
	if len(entry.RepoTags) == 0 {
		return fmt.Errorf("manifest entry has no RepoTags; untagged images cannot be loaded")
	}
	cfgPath := filepath.Join(stage, entry.Config)
	cfgBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read image config %q: %w", entry.Config, err)
	}
	var cfg saveImageConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return fmt.Errorf("parse image config: %w", err)
	}

	for _, ref := range entry.RepoTags {
		normRef := normalizeImageRef(ref)
		send(map[string]string{"status": fmt.Sprintf("Loading layer for image: %s", normRef)})
		dataset, err := h.createLoadedImageDataset(stage, entry.Layers, normRef)
		if err != nil {
			return fmt.Errorf("create dataset for %s: %w", normRef, err)
		}
		rec := &store.ImageRecord{
			ID:              "loaded_" + oci.SafeDirName(normRef),
			Ref:             normRef,
			Arch:            orDefault(cfg.Architecture, "amd64"),
			TemplateDataset: dataset,
			Created:         time.Now(),
			OCIEntrypoint:   append([]string{}, cfg.Config.Entrypoint...),
			OCICmd:          append([]string{}, cfg.Config.Cmd...),
			OCIEnv:          append([]string{}, cfg.Config.Env...),
			OCIWorkingDir:   cfg.Config.WorkingDir,
			OCIPorts:        mapKeys(cfg.Config.ExposedPorts),
			OCILabels:       cfg.Config.Labels,
			OCIUser:         cfg.Config.User,
			OCIStopSignal:   cfg.Config.StopSignal,
		}
		if hc := cfg.Config.Healthcheck; hc != nil && len(hc.Test) > 0 {
			rec.OCIHealthcheck = &store.HealthcheckConfig{
				Test:        append([]string{}, hc.Test...),
				Interval:    hc.Interval,
				Timeout:     hc.Timeout,
				StartPeriod: hc.StartPeriod,
				Retries:     hc.Retries,
			}
		}
		if err := h.store.AddImage(rec); err != nil {
			return fmt.Errorf("persist %s: %w", normRef, err)
		}
		h.publishEvent("image", "load", normRef, map[string]string{"name": normRef})
		send(map[string]string{"status": fmt.Sprintf("Loaded image: %s", normRef)})
	}
	return nil
}

// createLoadedImageDataset picks a fresh ZFS dataset name, creates it
// with an explicit mountpoint (the parent dataset inherits mountpoint=none,
// so children need their own), extracts every referenced layer tar into
// it in order, takes the @tmpl snapshot the rest of the daemon expects on
// templates, and returns the dataset path.
func (h *Handler) createLoadedImageDataset(stage string, layers []string, ref string) (string, error) {
	storage := h.mgr.PVEStorage()
	parentDS := storage + "/dld-templates"
	dataset := fmt.Sprintf("%s/%s", parentDS, oci.SafeDirName(ref))
	mountPoint := "/" + dataset
	// Mirror pullOCI: ensure the parent dataset exists and force the new
	// per-image dataset's mountpoint so /.zfs/snapshot/tmpl is reachable.
	_, _ = exec.Command("zfs", "create", "-p", "-o", "mountpoint=none", parentDS).CombinedOutput()
	// Idempotent re-load: destroy any stale dataset first so the @tmpl
	// snapshot is replaced cleanly.
	_, _ = exec.Command("zfs", "destroy", "-r", dataset).CombinedOutput()
	if out, err := exec.Command("zfs", "create", "-o", "mountpoint="+mountPoint, dataset).CombinedOutput(); err != nil {
		return "", fmt.Errorf("zfs create %s: %s: %w", dataset, out, err)
	}
	for _, layer := range layers {
		layerPath := filepath.Join(stage, layer)
		if err := extractTarInto(layerPath, mountPoint); err != nil {
			_, _ = exec.Command("zfs", "destroy", "-r", dataset).CombinedOutput()
			return "", fmt.Errorf("extract %s: %w", layer, err)
		}
	}
	if out, err := exec.Command("zfs", "snapshot", dataset+"@tmpl").CombinedOutput(); err != nil {
		_, _ = exec.Command("zfs", "destroy", "-r", dataset).CombinedOutput()
		return "", fmt.Errorf("zfs snapshot %s@tmpl: %s: %w", dataset, out, err)
	}
	return dataset, nil
}

// extractBundleTar unpacks a docker-save bundle into destDir. Used by
// loadImage before parsing the manifest — the archive/tar reader is
// stream-only, so this materialises the bundle to disk.
func extractBundleTar(body io.Reader, destDir string) error {
	tr := tar.NewReader(body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean("/" + hdr.Name)
		if strings.Contains(clean, "..") {
			return fmt.Errorf("invalid path %q in bundle", hdr.Name)
		}
		target := filepath.Join(destDir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}

// extractTarInto runs `tar -xf <src> -C <dst>` so symlinks, xattrs, and
// device files are handled correctly — pure-Go tar would have to
// reimplement half of GNU tar to get the same behaviour.
func extractTarInto(src, dst string) error {
	out, err := exec.Command("tar", "-xf", src, "-C", dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// mapKeys returns a sorted list of the keys of m. Used for the exposed-
// ports map which docker save emits as an object and the store keeps as
// a string slice.
func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
