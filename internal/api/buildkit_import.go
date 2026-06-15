package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
)

// buildViaLLB is the BuildKit Solve build engine: it converts the Dockerfile to
// a real LLB DAG, executes that DAG on LXC via llbExecutor, and imports the
// result as an image. It is wired as the controlServer's buildFn so
// `docker buildx build` uses true LLB semantics, while the classic POST /build
// handler keeps using the buildFromContext interpreter.
func (h *Handler) buildViaLLB(ctx context.Context, ctxDir, dockerfilePath, tag, target string, buildArgs, labels map[string]string, send func(any), getSecret secretFunc) (string, error) {
	resultDir, cleanup, img, err := h.buildLLBResult(ctx, ctxDir, dockerfilePath, target, buildArgs, labels, send, getSecret)
	if err != nil {
		return "", err
	}
	defer cleanup()

	ref := normalizeImageRef(tag)
	send(map[string]string{"stream": fmt.Sprintf("Exporting image %s\n", ref)})
	if err := h.importLLBResult(resultDir, ref, img); err != nil {
		return "", fmt.Errorf("export image: %w", err)
	}
	return ref, nil
}

// buildLLBResult converts the Dockerfile to LLB and executes it, returning the
// final result rootfs directory plus its image config. It does NOT import the
// result anywhere — the caller either imports it as an image (buildViaLLB) or
// streams it to the client (the tar/local exporters). cleanup removes the
// build scratch and must be called once the result has been consumed.
func (h *Handler) buildLLBResult(ctx context.Context, ctxDir, dockerfilePath, target string, buildArgs, labels map[string]string, send func(any), getSecret secretFunc) (resultDir string, cleanup func(), img *dockerspec.DockerOCIImage, err error) {
	dfAbs, err := safeJoin(ctxDir, dockerfilePath)
	if err != nil {
		return "", nil, nil, fmt.Errorf("invalid dockerfile path: %w", err)
	}
	dfBytes, err := os.ReadFile(dfAbs)
	if err != nil {
		return "", nil, nil, fmt.Errorf("read Dockerfile: %w", err)
	}

	emit := func(s string) { send(map[string]string{"stream": s}) }
	emit(fmt.Sprintf("Converting %s to LLB\n", dockerfilePath))

	def, image, err := dockerfileToLLB(ctx, dfBytes, target, buildArgs, labels, skopeoMetaResolver{h: h})
	if err != nil {
		return "", nil, nil, fmt.Errorf("dockerfile to LLB: %w", err)
	}

	resultDir, cleanup, err = h.solveLLB(ctx, ctxDir, def, emit, getSecret)
	if err != nil {
		return "", nil, nil, err
	}
	return resultDir, cleanup, image, nil
}

// importLLBResult registers a built rootfs directory as an LXC image. It stages
// the rootfs as a throwaway build container, then reuses finalizeBuiltImage so
// the import path (PVE zfs template vs. dir template) is identical to the
// classic builder.
func (h *Handler) importLLBResult(resultDir, ref string, img *dockerspec.DockerOCIImage) error {
	if existing := h.store.GetImage(ref); existing != nil {
		if err := h.mgr.RemoveImage(ref); err != nil {
			return fmt.Errorf("remove existing image %s: %w", ref, err)
		}
	}

	tmpID := "build-" + generateID()[:12]
	containerDir := filepath.Join(h.mgr.LXCPath(), tmpID)
	rootfs := filepath.Join(containerDir, "rootfs")
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		return err
	}
	if err := moveOrCopyTree(resultDir, rootfs); err != nil {
		os.RemoveAll(containerDir)
		return fmt.Errorf("stage build rootfs: %w", err)
	}

	rec := &store.ContainerRecord{
		ID:      tmpID,
		Name:    tmpID,
		Image:   "scratch",
		ImageID: "scratch",
		Created: time.Now(),
	}
	if err := h.store.AddContainer(rec); err != nil {
		os.RemoveAll(containerDir)
		return err
	}

	state := imageConfigToBuildState(img)
	if err := h.finalizeBuiltImage(tmpID, ref, state); err != nil {
		_ = h.store.RemoveContainer(tmpID)
		os.RemoveAll(containerDir)
		return err
	}
	return nil
}

// moveOrCopyTree relocates src to dst, falling back to a recursive copy when
// the two live on different filesystems (the executor's scratch is usually in
// /tmp while the LXC store is on a data pool).
func moveOrCopyTree(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	out, err := exec.Command("cp", "-a", src+"/.", dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// imageConfigToBuildState maps the OCI image config produced by dockerfile2llb
// into the buildState that finalizeBuiltImage persists as the image's runtime
// metadata (entrypoint, cmd, env, exposed ports, …).
func imageConfigToBuildState(img *dockerspec.DockerOCIImage) buildState {
	st := buildState{labels: map[string]string{}}
	if img == nil {
		return st
	}
	cfg := img.Config
	st.env = append([]string{}, cfg.Env...)
	st.entrypoint = append([]string{}, cfg.Entrypoint...)
	st.cmd = append([]string{}, cfg.Cmd...)
	st.workdir = cfg.WorkingDir
	st.user = cfg.User
	st.stopSignal = cfg.StopSignal
	st.shell = append([]string{}, cfg.Shell...)
	st.exposed = sortedKeys(cfg.ExposedPorts)
	st.volumes = sortedKeys(cfg.Volumes)
	for k, v := range cfg.Labels {
		st.labels[k] = v
	}
	if hc := cfg.Healthcheck; hc != nil {
		st.healthcheck = &store.HealthcheckConfig{
			Test:        append([]string{}, hc.Test...),
			Interval:    int64(hc.Interval),
			Timeout:     int64(hc.Timeout),
			StartPeriod: int64(hc.StartPeriod),
			Retries:     hc.Retries,
		}
	}
	return st
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
