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

// GET /images/{name}/get — Docker save: stream a tar bundle usable by
// `docker load` or Portainer's "Download image" button.
//
// The emitted tar carries:
//   - layer.tar         — the rootfs as a flat tar
//   - <configSHA>.json  — an OCI image config we synthesise from the store
//   - manifest.json     — [{"Config":"<sha>.json","RepoTags":[...],"Layers":["layer.tar"]}]
//   - repositories      — legacy v1 tag map so older docker CLIs load it
func (h *Handler) saveImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	rec := h.store.GetImage(normalizeImageRef(name))
	if rec == nil {
		errResponse(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", name))
		return
	}

	rootfs, cleanup, err := h.openImageRootfs(rec)
	if err != nil {
		errResponse(w, http.StatusInternalServerError, "resolve rootfs: "+err.Error())
		return
	}
	defer cleanup()

	// Stage the layer tar in a temp dir so we can hash it before emitting
	// the outer manifest (the manifest references layer.tar by its sha).
	stage, err := os.MkdirTemp("", "dld-save-*")
	if err != nil {
		errResponse(w, http.StatusInternalServerError, "stage: "+err.Error())
		return
	}
	defer os.RemoveAll(stage)

	layerPath := filepath.Join(stage, "layer.tar")
	layerSHA, err := writeLayerTar(r.Context(), rootfs, layerPath)
	if err != nil {
		errResponse(w, http.StatusInternalServerError, "tar rootfs: "+err.Error())
		return
	}

	configBytes, configSHA, err := synthesiseImageConfig(rec, layerSHA)
	if err != nil {
		errResponse(w, http.StatusInternalServerError, "synthesise config: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)

	tw := tar.NewWriter(w)
	defer tw.Close()

	// manifest.json
	manifest := []map[string]any{{
		"Config":   configSHA + ".json",
		"RepoTags": []string{rec.Ref},
		"Layers":   []string{"layer.tar"},
	}}
	manifestJSON, _ := json.Marshal(manifest)
	if err := writeTarFile(tw, "manifest.json", manifestJSON, 0o644); err != nil {
		return
	}

	// repositories — legacy v1 tag map (docker load still consumes it).
	repo, tag := splitImageRef(rec.Ref)
	repositories := map[string]map[string]string{
		repo: {tag: configSHA},
	}
	repositoriesJSON, _ := json.Marshal(repositories)
	if err := writeTarFile(tw, "repositories", repositoriesJSON, 0o644); err != nil {
		return
	}

	// <configSHA>.json
	if err := writeTarFile(tw, configSHA+".json", configBytes, 0o644); err != nil {
		return
	}

	// layer.tar (stream from staging)
	f, err := os.Open(layerPath)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	hdr := &tar.Header{
		Name:    "layer.tar",
		Size:    fi.Size(),
		Mode:    0o644,
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return
	}
	_, _ = io.Copy(tw, f)
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
