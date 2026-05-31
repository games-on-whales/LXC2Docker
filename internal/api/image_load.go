package api

// Docker image load (`docker load`, `docker buildx build --load`).
//
// This lives in the default (untagged) build — unlike the broader save/load
// surface in image_save.go, which is gated behind `legacy_api_extras`. The
// buildx docker-container driver exports the finished image as a docker-archive
// tar and POSTs it here; without a working load the build succeeds but the
// result never lands in the daemon's image store. Everything here is
// self-contained (own manifest/config types and helpers, `ld`-prefixed) so it
// has no dependency on the tagged file.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/oci"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
)

// ldManifestEntry is one entry of a docker-archive manifest.json.
type ldManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// ldImageConfig is the subset of a docker-archive image config we map onto an
// ImageRecord's OCI metadata.
type ldImageConfig struct {
	Architecture string `json:"architecture"`
	Config       struct {
		Env          []string            `json:"Env"`
		Cmd          []string            `json:"Cmd"`
		Entrypoint   []string            `json:"Entrypoint"`
		WorkingDir   string              `json:"WorkingDir"`
		User         string              `json:"User"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Volumes      map[string]struct{} `json:"Volumes"`
		Labels       map[string]string   `json:"Labels"`
		StopSignal   string              `json:"StopSignal"`
		Healthcheck  *struct {
			Test        []string `json:"Test"`
			Interval    int64    `json:"Interval"`
			Timeout     int64    `json:"Timeout"`
			StartPeriod int64    `json:"StartPeriod"`
			Retries     int      `json:"Retries"`
		} `json:"Healthcheck"`
	} `json:"config"`
}

// loadImageHandler implements POST /images/load. It accepts a docker-archive
// tar (docker save / buildx --load output) and registers each contained image
// as a template in the daemon's store. The response is newline-delimited JSON
// so clients can tail progress.
func (h *Handler) loadImageHandler(w http.ResponseWriter, r *http.Request) {
	if h.mgr.PVEStorage() == "" {
		errResponse(w, http.StatusNotImplemented,
			"image load requires a PVE storage pool; legacy directory mode is not supported")
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
		send(map[string]any{"error": msg, "errorDetail": map[string]string{"message": msg}})
	}

	// docker save emits manifest.json first in most versions but not all, so
	// materialise the whole bundle to disk and read entries non-sequentially.
	stage, err := os.MkdirTemp("", "dld-load-*")
	if err != nil {
		fail("stage: " + err.Error())
		return
	}
	defer os.RemoveAll(stage)

	if err := ldExtractBundle(r.Body, stage); err != nil {
		fail("unpack bundle: " + err.Error())
		return
	}

	manifestBytes, err := os.ReadFile(filepath.Join(stage, "manifest.json"))
	if err != nil {
		fail("missing manifest.json")
		return
	}
	var manifest []ldManifestEntry
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		fail("parse manifest: " + err.Error())
		return
	}
	if len(manifest) == 0 {
		fail("manifest is empty")
		return
	}

	for _, entry := range manifest {
		if err := h.ldImportEntry(stage, entry, send); err != nil {
			fail(err.Error())
			return
		}
	}
}

// ldImportEntry materialises one manifest entry into a storage-appropriate
// template and persists the ImageRecord.
func (h *Handler) ldImportEntry(stage string, entry ldManifestEntry, send func(any)) error {
	if len(entry.Layers) == 0 {
		return fmt.Errorf("manifest entry has no layers")
	}
	if len(entry.RepoTags) == 0 {
		return fmt.Errorf("manifest entry has no RepoTags; untagged images cannot be loaded")
	}
	cfgBytes, err := os.ReadFile(filepath.Join(stage, entry.Config))
	if err != nil {
		return fmt.Errorf("read image config %q: %w", entry.Config, err)
	}
	var cfg ldImageConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return fmt.Errorf("parse image config: %w", err)
	}

	for _, ref := range entry.RepoTags {
		normRef := normalizeImageRef(ref)
		send(map[string]string{"status": fmt.Sprintf("Loading image: %s", normRef)})

		rec := &store.ImageRecord{
			ID:            "loaded_" + oci.SafeDirName(normRef),
			Ref:           normRef,
			Arch:          ldOrDefault(cfg.Architecture, "amd64"),
			Created:       time.Now(),
			OCIEntrypoint: append([]string{}, cfg.Config.Entrypoint...),
			OCICmd:        append([]string{}, cfg.Config.Cmd...),
			OCIEnv:        append([]string{}, cfg.Config.Env...),
			OCIWorkingDir: cfg.Config.WorkingDir,
			OCIPorts:      ldMapKeys(cfg.Config.ExposedPorts),
			OCILabels:     cfg.Config.Labels,
			OCIUser:       cfg.Config.User,
			OCIVolumes:    ldMapKeys(cfg.Config.Volumes),
			OCIStopSignal: cfg.Config.StopSignal,
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

		// Materialise the image as a template appropriate to the storage
		// backend: a ZFS dataset (+@tmpl snapshot) on zfs, or a pct-managed CT
		// template on lvmthin/dir/etc. createPVEContainer clones either.
		if h.mgr.PVEStorageIsZFS() {
			ds, err := h.ldCreateDatasetZFS(stage, entry.Layers, normRef)
			if err != nil {
				return fmt.Errorf("create dataset for %s: %w", normRef, err)
			}
			rec.TemplateDataset = ds
		} else {
			vmid, err := h.ldCreateTemplatePVE(stage, entry.Layers, normRef)
			if err != nil {
				return fmt.Errorf("create template for %s: %w", normRef, err)
			}
			rec.TemplateVMID = vmid
		}

		if err := h.store.AddImage(rec); err != nil {
			return fmt.Errorf("persist %s: %w", normRef, err)
		}
		send(map[string]string{"status": fmt.Sprintf("Loaded image: %s", normRef)})
	}
	return nil
}

// ldCreateDatasetZFS extracts the image layers into a fresh ZFS dataset and
// snapshots @tmpl (the form templates take on ZFS storage).
func (h *Handler) ldCreateDatasetZFS(stage string, layers []string, ref string) (string, error) {
	storage := h.mgr.PVEStorage()
	parentDS := storage + "/dld-templates"
	dataset := fmt.Sprintf("%s/%s", parentDS, oci.SafeDirName(ref))
	mountPoint := "/" + dataset
	_, _ = exec.Command("zfs", "create", "-p", "-o", "mountpoint=none", parentDS).CombinedOutput()
	_, _ = exec.Command("zfs", "destroy", "-r", dataset).CombinedOutput()
	if out, err := exec.Command("zfs", "create", "-o", "mountpoint="+mountPoint, dataset).CombinedOutput(); err != nil {
		return "", fmt.Errorf("zfs create %s: %s: %w", dataset, out, err)
	}
	for _, layer := range layers {
		if err := ldExtractTarInto(filepath.Join(stage, layer), mountPoint); err != nil {
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

// ldCreateTemplatePVE reconstructs the image rootfs from its layers (in order)
// and creates a pct-managed PVE template from it — the storage-agnostic form
// used on lvmthin/dir backends.
func (h *Handler) ldCreateTemplatePVE(stage string, layers []string, ref string) (int, error) {
	rootfs, err := os.MkdirTemp("", "dld-load-rootfs-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(rootfs)
	for _, layer := range layers {
		if err := ldExtractTarInto(filepath.Join(stage, layer), rootfs); err != nil {
			return 0, fmt.Errorf("extract %s: %w", layer, err)
		}
	}
	return h.mgr.CreatePVETemplateFromRootfs(ref, rootfs)
}

// ldExtractBundle unpacks a docker-archive tar (streamed from the request
// body) into destDir so manifest.json and the layer/config files can be read
// non-sequentially.
func ldExtractBundle(body io.Reader, destDir string) error {
	cmd := exec.Command("tar", "-xf", "-", "-C", destDir)
	cmd.Stdin = body
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", string(out), err)
	}
	return nil
}

// ldExtractTarInto extracts a layer tar into dst via GNU tar (so symlinks,
// xattrs, and device nodes are handled correctly).
func ldExtractTarInto(src, dst string) error {
	if out, err := exec.Command("tar", "-xf", src, "-C", dst).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", string(out), err)
	}
	return nil
}

func ldOrDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func ldMapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
