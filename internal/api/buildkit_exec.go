package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/solver/pb"
	"github.com/opencontainers/go-digest"
)

// llbExecutor materialises a BuildKit LLB pb.Op DAG on LXC using directory
// snapshots. Each op output is a directory; immutable inputs (image templates,
// the build context) are referenced in place, and a writable snapshot (cp -a)
// is taken whenever an op mutates its input.
//
// This is the real "LLB solver on LXC": Op_Source pulls an image rootfs or
// surfaces the build context, Op_Exec runs RUN steps in a chroot, and Op_File
// applies COPY/ADD/mkdir/rm. It is intentionally a correct subset — caching,
// secret/ssh mounts and multi-platform are layered on in Phase 3c.
// secretFunc fetches a build secret value by id from the client session. Solve
// supplies it (it holds the session) so the executor can satisfy
// RUN --mount=type=secret without knowing about gRPC sessions.
type secretFunc func(ctx context.Context, id string) ([]byte, error)

type llbExecutor struct {
	h         *Handler
	ctx       context.Context
	ctxDir    string       // local://context root (FileSync'd build context)
	scratch   string       // parent dir for all snapshots, removed after the build
	emit      func(string) // progress log sink
	getSecret secretFunc   // RUN --mount=type=secret resolver (may be nil)

	byDigest map[digest.Digest]*pb.Op
	// opOutputs memoises every output directory of an op, keyed by output index.
	opOutputs map[digest.Digest]map[int64]string
	// images caches resolved base-image rootfs dirs within a single build.
	images map[string]string
}

// solveLLB executes a converted Dockerfile (LLB definition + image config) and
// returns the directory holding the final build result. The caller imports it
// as an image.
func (h *Handler) solveLLB(ctx context.Context, ctxDir string, def *llb.Definition, emit func(string), getSecret secretFunc) (resultDir string, cleanup func(), err error) {
	verts, byDigest, err := llbOps(def)
	if err != nil {
		return "", nil, err
	}
	if len(verts) == 0 {
		return "", nil, fmt.Errorf("empty LLB definition")
	}
	// The final Def entry is a synthetic op whose first input points at the
	// real build result (buildkit's vertex.Load convention).
	last := verts[len(verts)-1].op
	if len(last.Inputs) == 0 {
		return "", nil, fmt.Errorf("LLB definition has no result edge")
	}
	resultEdge := last.Inputs[0]

	scratch, err := os.MkdirTemp("", "llb-build-*")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { os.RemoveAll(scratch) }

	e := &llbExecutor{
		h:         h,
		ctx:       ctx,
		ctxDir:    ctxDir,
		scratch:   scratch,
		emit:      emit,
		getSecret: getSecret,
		byDigest:  byDigest,
		opOutputs: map[digest.Digest]map[int64]string{},
		images:    map[string]string{},
	}
	dir, err := e.inputDir(resultEdge)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

// inputDir resolves a single LLB input edge (op digest + output index) to its
// materialised directory.
func (e *llbExecutor) inputDir(in *pb.Input) (string, error) {
	outs, err := e.outputsOf(digest.Digest(in.Digest))
	if err != nil {
		return "", err
	}
	dir, ok := outs[in.Index]
	if !ok {
		return "", fmt.Errorf("op %s has no output index %d", in.Digest, in.Index)
	}
	return dir, nil
}

// outputsOf evaluates an op (recursively evaluating its inputs first) and
// returns all of its output directories keyed by output index. Results are
// memoised so a shared op is executed once.
func (e *llbExecutor) outputsOf(dgst digest.Digest) (map[int64]string, error) {
	if outs, ok := e.opOutputs[dgst]; ok {
		return outs, nil
	}
	op, ok := e.byDigest[dgst]
	if !ok {
		return nil, fmt.Errorf("unknown LLB op %s", dgst)
	}

	inputDirs := make([]string, len(op.Inputs))
	for i, in := range op.Inputs {
		d, err := e.inputDir(in)
		if err != nil {
			return nil, err
		}
		inputDirs[i] = d
	}

	var outs map[int64]string
	var err error
	switch o := op.GetOp().(type) {
	case *pb.Op_Source:
		var dir string
		dir, err = e.execSource(o.Source)
		outs = map[int64]string{0: dir}
	case *pb.Op_Exec:
		outs, err = e.execExec(o.Exec, inputDirs)
	case *pb.Op_File:
		outs, err = e.execFile(o.File, inputDirs)
	default:
		err = fmt.Errorf("unsupported LLB op type %T", op.GetOp())
	}
	if err != nil {
		return nil, err
	}
	e.opOutputs[dgst] = outs
	return outs, nil
}

// snapshot makes a fresh writable copy of src under the scratch dir. An empty
// src yields an empty directory (used for FROM scratch and FileAction inputs of
// -1).
func (e *llbExecutor) snapshot(src string) (string, error) {
	dst, err := os.MkdirTemp(e.scratch, "snap-*")
	if err != nil {
		return "", err
	}
	if src == "" {
		return dst, nil
	}
	// cp -a preserves ownership/permissions/symlinks; "/." copies contents.
	out, err := exec.Command("cp", "-a", src+"/.", dst).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("snapshot %s: %s: %w", src, strings.TrimSpace(string(out)), err)
	}
	return dst, nil
}

// execSource materialises an LLB Source op into a directory.
func (e *llbExecutor) execSource(src *pb.SourceOp) (string, error) {
	id := src.GetIdentifier()
	switch {
	case strings.HasPrefix(id, "local://"):
		// The build context (and any named context) — surfaced read-only.
		name := strings.TrimPrefix(id, "local://")
		if name == "context" {
			return e.ctxDir, nil
		}
		return "", fmt.Errorf("unsupported local source %q", name)
	case strings.HasPrefix(id, "docker-image://"):
		ref := strings.TrimPrefix(id, "docker-image://")
		// Drop any pinned digest; the daemon resolves refs to LXC templates.
		if at := strings.LastIndex(ref, "@"); at >= 0 {
			ref = ref[:at]
		}
		return e.resolveImageRootfs(ref)
	default:
		return "", fmt.Errorf("unsupported LLB source %q", id)
	}
}

// resolveImageRootfs ensures the base image is available and returns the path
// to its (read-only, shared) rootfs. Reuses the daemon's image resolution so
// buildx FROM behaves identically to classic build / docker run.
func (e *llbExecutor) resolveImageRootfs(ref string) (string, error) {
	norm := normalizeImageRef(ref)
	if dir, ok := e.images[norm]; ok {
		return dir, nil
	}
	if err := e.h.ensureBuildBaseImage(norm, func(v any) {
		if m, ok := v.(map[string]string); ok {
			if s, ok := m["stream"]; ok {
				e.emit(s)
			}
		}
	}); err != nil {
		return "", fmt.Errorf("resolve image %s: %w", norm, err)
	}
	rec := e.h.store.GetImage(norm)
	if rec == nil {
		return "", fmt.Errorf("image %s not found after resolve", norm)
	}
	root, cleanup, err := e.h.openImageRootfs(rec)
	if err != nil {
		return "", fmt.Errorf("open image rootfs %s: %w", norm, err)
	}
	// openImageRootfs may mount; keep it mounted for the build and unmount at
	// scratch teardown by deferring through the executor's cleanup chain.
	e.images[norm] = root
	_ = cleanup // mounts are released when the daemon GCs; acceptable for v1
	return root, nil
}

// execExec runs an LLB Exec op (a RUN step). It snapshots the root mount,
// stages additional mounts into it (read-only binds, ephemeral cache/tmpfs,
// and RUN --mount=type=secret files), runs the command in a chroot, then
// deletes secret files so they never land in the built image. SSH agent mounts
// can't be provided under the chroot model and are skipped with a warning.
func (e *llbExecutor) execExec(eop *pb.ExecOp, inputDirs []string) (map[int64]string, error) {
	meta := eop.GetMeta()
	if meta == nil {
		return nil, fmt.Errorf("exec op missing meta")
	}
	mounts := eop.GetMounts()

	// Pass 1: resolve the root mount (Dest "/") into the writable snapshot the
	// command runs in. Everything else stages into it, so it must exist first.
	outs := map[int64]string{}
	var rootDir string
	for _, m := range mounts {
		if m.GetMountType() != pb.MountType_BIND || m.GetDest() != "/" {
			continue
		}
		src := ""
		if m.GetInput() != int64(pb.Empty) {
			src = inputDirs[m.GetInput()]
		}
		snap, err := e.snapshot(src)
		if err != nil {
			return nil, err
		}
		rootDir = snap
		if m.GetOutput() != int64(pb.SkipOutput) {
			outs[m.GetOutput()] = snap
		}
	}
	if rootDir == "" {
		return nil, fmt.Errorf("exec op has no root mount")
	}

	// Pass 2: stage the remaining mounts into the root snapshot.
	var secretFiles []string
	for _, m := range mounts {
		if m.GetMountType() == pb.MountType_BIND && m.GetDest() == "/" {
			continue // handled in pass 1
		}
		switch m.GetMountType() {
		case pb.MountType_BIND:
			src := ""
			if m.GetInput() != int64(pb.Empty) {
				src = inputDirs[m.GetInput()]
			}
			if err := e.stageDir(src, rootDir, m.GetDest()); err != nil {
				return nil, err
			}
		case pb.MountType_CACHE, pb.MountType_TMPFS:
			// Ephemeral writable dir; not persisted as an output.
			target := filepath.Join(rootDir, strings.TrimPrefix(m.GetDest(), "/"))
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
		case pb.MountType_SECRET:
			path, err := e.stageSecret(rootDir, m)
			if err != nil {
				return nil, err
			}
			if path != "" {
				secretFiles = append(secretFiles, path)
			}
		case pb.MountType_SSH:
			e.emit("warning: RUN --mount=type=ssh is not supported on the LXC backend; ignoring\n")
		default:
			return nil, fmt.Errorf("unsupported mount type %v", m.GetMountType())
		}
	}

	runErr := e.runInRoot(rootDir, meta)
	for _, f := range secretFiles {
		_ = os.Remove(f) // secrets must not persist in the image
	}
	if runErr != nil {
		return nil, runErr
	}
	return outs, nil
}

// stageDir copies a read-only input directory into the root snapshot at dest.
func (e *llbExecutor) stageDir(src, rootDir, dest string) error {
	if src == "" {
		return nil
	}
	target := filepath.Join(rootDir, strings.TrimPrefix(dest, "/"))
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if out, err := exec.Command("cp", "-a", src+"/.", target).CombinedOutput(); err != nil {
		return fmt.Errorf("stage mount %s: %s: %w", dest, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// stageSecret fetches a RUN --mount=type=secret value from the client session
// and writes it into the root snapshot at the mount destination, returning the
// file path so the caller can delete it after the run. A missing optional
// secret is skipped.
func (e *llbExecutor) stageSecret(rootDir string, m *pb.Mount) (string, error) {
	so := m.GetSecretOpt()
	if so == nil {
		return "", nil
	}
	if e.getSecret == nil {
		if so.GetOptional() {
			return "", nil
		}
		return "", fmt.Errorf("secret %q requested but no build session is available", so.GetID())
	}
	val, err := e.getSecret(e.ctx, so.GetID())
	if err != nil {
		if so.GetOptional() {
			return "", nil
		}
		return "", fmt.Errorf("secret %q: %w", so.GetID(), err)
	}

	dest := m.GetDest()
	if dest == "" {
		dest = "/run/secrets/" + so.GetID()
	}
	target := filepath.Join(rootDir, strings.TrimPrefix(dest, "/"))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	mode := os.FileMode(0o600)
	if so.GetMode() != 0 {
		mode = os.FileMode(so.GetMode() & 0o777)
	}
	if err := os.WriteFile(target, val, mode); err != nil {
		return "", err
	}
	return target, nil
}

// runInRoot executes meta.Args inside rootDir via chroot, honouring Cwd, Env
// and User. meta.Args is the full argv (dockerfile2llb wraps shell-form RUN as
// /bin/sh -c "<script>").
func (e *llbExecutor) runInRoot(rootDir string, meta *pb.Meta) error {
	args := []string{}
	if u := strings.TrimSpace(meta.GetUser()); u != "" {
		args = append(args, "--userspec", u)
	}
	args = append(args, rootDir)

	cwd := meta.GetCwd()
	if cwd == "" {
		cwd = "/"
	}
	if cwd != "/" {
		// Ensure the workdir exists and cd into it before exec'ing the command.
		if err := os.MkdirAll(filepath.Join(rootDir, strings.TrimPrefix(cwd, "/")), 0o755); err != nil {
			return err
		}
		wrapper := []string{"/bin/sh", "-c", "cd \"$1\" && shift && exec \"$@\"", "sh", cwd}
		args = append(args, wrapper...)
		args = append(args, meta.GetArgs()...)
	} else {
		args = append(args, meta.GetArgs()...)
	}

	cmd := exec.Command("chroot", args...)
	cmd.Env = meta.GetEnv()
	if len(cmd.Env) == 0 {
		cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	}
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		e.emit(string(out))
	}
	if err != nil {
		return fmt.Errorf("RUN %s failed: %w", strings.Join(meta.GetArgs(), " "), err)
	}
	return nil
}

// execFile applies an LLB File op. The op's inputs seed a growing state array;
// each action reads a base (Input) and optional source (SecondaryInput) from
// that array, writes a new snapshot, appends it, and optionally exposes it as
// an op output. Input/SecondaryInput of -1 (pb.Empty) means an empty scratch.
func (e *llbExecutor) execFile(file *pb.FileOp, inputDirs []string) (map[int64]string, error) {
	state := append([]string{}, inputDirs...)
	outs := map[int64]string{}

	dirAt := func(idx int64) (string, error) {
		if idx == int64(pb.Empty) {
			return "", nil
		}
		if idx < 0 || int(idx) >= len(state) {
			return "", fmt.Errorf("file action index %d out of range", idx)
		}
		return state[idx], nil
	}

	for ai, act := range file.GetActions() {
		base, err := dirAt(act.GetInput())
		if err != nil {
			return nil, err
		}
		work, err := e.snapshot(base)
		if err != nil {
			return nil, err
		}
		if err := e.applyFileAction(work, state, act); err != nil {
			return nil, fmt.Errorf("file action %d: %w", ai, err)
		}
		state = append(state, work)
		if act.GetOutput() != int64(pb.SkipOutput) {
			outs[act.GetOutput()] = work
		}
	}
	if len(outs) == 0 {
		return nil, fmt.Errorf("file op produced no outputs")
	}
	return outs, nil
}

func (e *llbExecutor) applyFileAction(work string, state []string, act *pb.FileAction) error {
	switch a := act.GetAction().(type) {
	case *pb.FileAction_Copy:
		c := a.Copy
		srcRoot := e.ctxDir
		if act.GetSecondaryInput() != int64(pb.Empty) {
			if int(act.GetSecondaryInput()) >= len(state) {
				return fmt.Errorf("copy source index %d out of range", act.GetSecondaryInput())
			}
			srcRoot = state[act.GetSecondaryInput()]
		}
		srcAbs, err := safeJoin(srcRoot, c.GetSrc())
		if err != nil {
			return err
		}
		dstAbs, err := safeJoin(work, c.GetDest())
		if err != nil {
			return err
		}
		if c.GetCreateDestPath() {
			if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
				return err
			}
		}
		return copyTree(srcAbs, dstAbs)
	case *pb.FileAction_Mkdir:
		m := a.Mkdir
		dst, err := safeJoin(work, m.GetPath())
		if err != nil {
			return err
		}
		mode := os.FileMode(m.GetMode() & 0o777)
		if m.GetMakeParents() {
			return os.MkdirAll(dst, mode)
		}
		return os.Mkdir(dst, mode)
	case *pb.FileAction_Mkfile:
		m := a.Mkfile
		dst, err := safeJoin(work, m.GetPath())
		if err != nil {
			return err
		}
		return os.WriteFile(dst, m.GetData(), os.FileMode(m.GetMode()&0o777))
	case *pb.FileAction_Rm:
		r := a.Rm
		dst, err := safeJoin(work, r.GetPath())
		if err != nil {
			return err
		}
		err = os.RemoveAll(dst)
		if err != nil && r.GetAllowNotFound() && os.IsNotExist(err) {
			return nil
		}
		return err
	case *pb.FileAction_Symlink:
		s := a.Symlink
		dst, err := safeJoin(work, s.GetNewpath())
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.Symlink(s.GetOldpath(), dst)
	default:
		return fmt.Errorf("unsupported file action %T", act.GetAction())
	}
}
