package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	bktypes "github.com/moby/buildkit/api/types"
	"github.com/moby/buildkit/client/buildid"
	"github.com/moby/buildkit/client/llb"
	exptypes "github.com/moby/buildkit/exporter/containerimage/exptypes"
	gwpb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/session/secrets"
	opspb "github.com/moby/buildkit/solver/pb"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	digest "github.com/opencontainers/go-digest"
	fstypes "github.com/tonistiigi/fsutil/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gatewayBuild is the per-build state for a gateway-mode Solve. buildx's docker
// driver drives the Dockerfile frontend client-side, calling the daemon's
// LLBBridge (Solve/ReadFile/Return/…) over the same /grpc connection. The outer
// Control.Solve registers this, waits for Return, then exports the result.
type gatewayBuild struct {
	sessionID string

	mu        sync.Mutex
	localDirs map[string]string // FileSync'd "context"/"dockerfile" dirs (fetched once)
	cleanups  []func()
	refs      map[string]string // LLBBridge result ref id -> materialised dir

	done     chan struct{}
	doneOnce sync.Once
	result   string            // ref id returned via Return
	metadata map[string][]byte // Return result metadata (carries image config)
	retErr   error
}

func (b *gatewayBuild) finish(ref string, md map[string][]byte, err error) {
	b.doneOnce.Do(func() {
		b.mu.Lock()
		b.result, b.metadata, b.retErr = ref, md, err
		b.mu.Unlock()
		close(b.done)
	})
}

func (b *gatewayBuild) addCleanup(fn func()) {
	b.mu.Lock()
	b.cleanups = append(b.cleanups, fn)
	b.mu.Unlock()
}

func (b *gatewayBuild) cleanupAll() {
	b.mu.Lock()
	fns := b.cleanups
	b.cleanups = nil
	b.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// ensureLocals FileSyncs the build context and Dockerfile dir from the client
// session into temp dirs, once per build. Returns the local://<name> dir map.
func (s *controlServer) ensureLocals(ctx context.Context, b *gatewayBuild) (map[string]string, error) {
	b.mu.Lock()
	if b.localDirs != nil {
		ld := b.localDirs
		b.mu.Unlock()
		return ld, nil
	}
	b.mu.Unlock()

	caller, err := s.sm.Get(ctx, b.sessionID, false)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	ld := map[string]string{}
	for _, name := range []string{"context", "dockerfile"} {
		dir, err := os.MkdirTemp("", "gw-"+name+"-*")
		if err != nil {
			return nil, err
		}
		b.addCleanup(func() { os.RemoveAll(dir) })
		if err := filesync.FSSync(ctx, caller, filesync.FSSendRequestOpt{Name: name, DestDir: dir}); err != nil {
			// A build may not expose both; leave the dir empty rather than fail.
			continue
		}
		ld[name] = dir
	}
	b.mu.Lock()
	b.localDirs = ld
	b.mu.Unlock()
	return ld, nil
}

// solveGateway handles a Control.Solve with an empty frontend — buildx's
// gateway build. The real build's client-side buildFunc drives our LLBBridge
// (Solve(Frontend="dockerfile.v0") → Return); this coordinator registers the
// build, waits for Return, then exports the returned ref's rootfs.
func (s *controlServer) solveGateway(ctx context.Context, req *controlapi.SolveRequest) (*controlapi.SolveResponse, error) {
	// buildx probes each node's BuildOpts/caps with an Internal build whose
	// buildFunc just calls BuildOpts() and returns nil (build/resolver). Treat
	// it as a no-op success — anything else aborts the build before it starts.
	if req.Internal {
		return &controlapi.SolveResponse{ExporterResponse: map[string]string{}}, nil
	}

	b := &gatewayBuild{
		sessionID: req.Session,
		refs:      map[string]string{},
		done:      make(chan struct{}),
	}
	s.gwMu.Lock()
	s.gwBuilds[req.Ref] = b
	s.gwMu.Unlock()
	defer func() {
		s.gwMu.Lock()
		delete(s.gwBuilds, req.Ref)
		s.gwMu.Unlock()
		b.cleanupAll()
	}()
	st := s.statusFor(req.Ref)
	defer close(st.ch)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.done:
	}
	if b.retErr != nil {
		return nil, b.retErr
	}

	b.mu.Lock()
	dir := b.refs[b.result]
	md := b.metadata
	b.mu.Unlock()
	if dir == "" {
		return nil, status.Error(codes.Internal, "gateway build returned no result ref")
	}

	resp := map[string]string{}
	if cfg := md[exptypes.ExporterImageConfigKey]; len(cfg) > 0 {
		resp[exptypes.ExporterImageConfigKey] = string(cfg)
	}
	if tag := exporterImageName(req); tag != "" {
		if err := s.h.importLLBResult(dir, tag, imageConfigFromMetadata(md)); err != nil {
			return nil, status.Errorf(codes.Internal, "import build result: %v", err)
		}
	}
	return &controlapi.SolveResponse{ExporterResponse: resp}, nil
}

// imageConfigFromMetadata extracts the OCI image config the frontend places in
// the result metadata, so the imported image carries its Cmd/Env/etc.
func imageConfigFromMetadata(md map[string][]byte) *dockerspec.DockerOCIImage {
	raw, ok := md[exptypes.ExporterImageConfigKey]
	if !ok || len(raw) == 0 {
		return nil
	}
	var img dockerspec.DockerOCIImage
	if err := json.Unmarshal(raw, &img); err != nil {
		return nil
	}
	return &img
}

// gatewayBridge serves the BuildKit LLBBridge gRPC service. It's a distinct type
// from controlServer because both services declare a Solve method (with
// different request types), which can't coexist on one Go type.
type gatewayBridge struct {
	gwpb.UnimplementedLLBBridgeServer
	cs *controlServer
}

func (g *gatewayBridge) build(ctx context.Context) (*gatewayBuild, error) {
	id := buildid.FromIncomingContext(ctx)
	// The client-side buildFunc (LLBBridge calls) and the outer Control.Solve
	// (which registers the build) run concurrently, so the first LLBBridge call
	// can arrive before solveGateway registers. Wait briefly for registration.
	deadline := time.Now().Add(15 * time.Second)
	for {
		g.cs.gwMu.Lock()
		b := g.cs.gwBuilds[id]
		g.cs.gwMu.Unlock()
		if b != nil {
			return b, nil
		}
		if time.Now().After(deadline) {
			return nil, status.Errorf(codes.NotFound, "no gateway build for id %q", id)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (g *gatewayBridge) Inputs(ctx context.Context, req *gwpb.InputsRequest) (*gwpb.InputsResponse, error) {
	return &gwpb.InputsResponse{}, nil
}

func (g *gatewayBridge) Ping(context.Context, *gwpb.PingRequest) (*gwpb.PongResponse, error) {
	return &gwpb.PongResponse{
		FrontendAPICaps: gwpb.Caps.All(),
		LLBCaps:         opspb.Caps.All(),
		Workers: []*bktypes.WorkerRecord{{
			ID:        "lxc",
			Platforms: []*opspb.Platform{{Architecture: "amd64", OS: "linux"}},
		}},
	}, nil
}

// ResolveImageConfig returns the OCI config of a base image (FROM). The daemon
// ensures the image is pulled and reconstructs its config from the image
// record; the digest is synthetic (the executor resolves images by ref).
func (g *gatewayBridge) ResolveImageConfig(ctx context.Context, req *gwpb.ResolveImageConfigRequest) (*gwpb.ResolveImageConfigResponse, error) {
	norm := normalizeImageRef(req.Ref)
	if err := g.cs.h.ensureBuildBaseImage(norm, func(any) {}); err != nil {
		return nil, status.Errorf(codes.NotFound, "resolve image %s: %v", norm, err)
	}
	rec := g.cs.h.store.GetImage(norm)
	if rec == nil {
		return nil, status.Errorf(codes.NotFound, "image %s not found after pull", norm)
	}
	cfg, err := json.Marshal(imageRecordToOCIImage(rec))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal image config: %v", err)
	}
	return &gwpb.ResolveImageConfigResponse{
		Digest: digest.FromBytes(cfg).String(),
		Config: cfg,
	}, nil
}

// Solve executes the LLB definition the client built (the Dockerfile frontend
// lowers RUN/COPY/FROM to LLB), materialises the result, and hands back a ref id
// the client can ReadFile from or Return.
func (g *gatewayBridge) Solve(ctx context.Context, req *gwpb.SolveRequest) (*gwpb.SolveResponse, error) {
	b, err := g.build(ctx)
	if err != nil {
		return nil, err
	}

	// buildx's docker driver asks the daemon to run the Dockerfile frontend by
	// calling Solve with Frontend="dockerfile.v0" (and no Definition). Run the
	// build server-side and return the result ref plus the image config.
	if req.Frontend == dockerfileFrontend {
		return g.solveDockerfileFrontend(ctx, b, req)
	}

	if req.Definition == nil || len(req.Definition.Def) == 0 {
		if req.Frontend != "" {
			return nil, status.Errorf(codes.Unimplemented, "unsupported frontend %q", req.Frontend)
		}
		// An empty solve (e.g. scratch) yields an empty dir.
		dir, derr := os.MkdirTemp("", "gw-empty-*")
		if derr != nil {
			return nil, derr
		}
		b.addCleanup(func() { os.RemoveAll(dir) })
		id := "ref-" + generateID()[:12]
		b.mu.Lock()
		b.refs[id] = dir
		b.mu.Unlock()
		return &gwpb.SolveResponse{Ref: id}, nil
	}

	localDirs, err := g.cs.ensureLocals(ctx, b)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetch build context: %v", err)
	}

	def := &llb.Definition{Def: req.Definition.Def}
	emit := func(string) {} // gateway progress is carried by the frontend itself
	getSecret := func(sctx context.Context, sid string) ([]byte, error) {
		caller, gerr := g.cs.sm.Get(sctx, b.sessionID, false)
		if gerr != nil {
			return nil, gerr
		}
		return secrets.GetSecret(sctx, caller, sid)
	}

	dir, cleanup, err := g.cs.h.solveLLBLocals(ctx, localDirs, def, emit, getSecret)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "solve: %v", err)
	}
	b.addCleanup(cleanup)

	id := "ref-" + generateID()[:12]
	b.mu.Lock()
	b.refs[id] = dir
	b.mu.Unlock()
	return &gwpb.SolveResponse{Ref: id}, nil
}

// solveDockerfileFrontend runs the Dockerfile frontend server-side: it fetches
// the build context + Dockerfile from the client session, runs the LLB build,
// and returns a result ref whose Result carries the image config in metadata
// (the exptypes key buildx reads to export the image).
func (g *gatewayBridge) solveDockerfileFrontend(ctx context.Context, b *gatewayBuild, req *gwpb.SolveRequest) (*gwpb.SolveResponse, error) {
	localDirs, err := g.cs.ensureLocals(ctx, b)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetch build context: %v", err)
	}
	ctxDir := localDirs["context"]
	if ctxDir == "" {
		return nil, status.Error(codes.Internal, "build context not provided over session")
	}

	dfName := req.FrontendOpt["filename"]
	if dfName == "" {
		dfName = "Dockerfile"
	}
	// The Dockerfile may have synced into the "dockerfile" local rather than the
	// context; stage it into the context dir so buildLLBResult finds it.
	if _, statErr := os.Stat(filepath.Join(ctxDir, dfName)); statErr != nil {
		if dfDir := localDirs["dockerfile"]; dfDir != "" {
			if data, rerr := os.ReadFile(filepath.Join(dfDir, dfName)); rerr == nil {
				_ = os.WriteFile(filepath.Join(ctxDir, dfName), data, 0o644)
			}
		}
	}

	target := req.FrontendOpt["target"]
	buildArgs, labels := frontendBuildArgsAndLabels(req.FrontendOpt)
	send := func(any) {}
	getSecret := func(sctx context.Context, sid string) ([]byte, error) {
		caller, gerr := g.cs.sm.Get(sctx, b.sessionID, false)
		if gerr != nil {
			return nil, gerr
		}
		return secrets.GetSecret(sctx, caller, sid)
	}

	dir, cleanup, img, err := g.cs.h.buildLLBResult(ctx, ctxDir, dfName, target, buildArgs, labels, send, getSecret)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "dockerfile build: %v", err)
	}
	b.addCleanup(cleanup)

	id := "ref-" + generateID()[:12]
	b.mu.Lock()
	b.refs[id] = dir
	b.mu.Unlock()

	md := map[string][]byte{}
	if img != nil {
		if cfg, mErr := json.Marshal(img); mErr == nil {
			md[exptypes.ExporterImageConfigKey] = cfg
		}
	}
	return &gwpb.SolveResponse{
		Ref: id,
		Result: &gwpb.Result{
			Result:   &gwpb.Result_Ref{Ref: &gwpb.Ref{Id: id}},
			Metadata: md,
		},
	}, nil
}

func (g *gatewayBridge) refDir(ctx context.Context, refID string) (string, error) {
	b, err := g.build(ctx)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	dir, ok := b.refs[refID]
	b.mu.Unlock()
	if !ok {
		return "", status.Errorf(codes.NotFound, "unknown ref %q", refID)
	}
	return dir, nil
}

func (g *gatewayBridge) ReadFile(ctx context.Context, req *gwpb.ReadFileRequest) (*gwpb.ReadFileResponse, error) {
	dir, err := g.refDir(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, req.FilePath))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "read %s: %v", req.FilePath, err)
	}
	if r := req.Range; r != nil {
		start := int(r.Offset)
		if start > len(data) {
			start = len(data)
		}
		end := start + int(r.Length)
		if r.Length < 0 || end > len(data) {
			end = len(data)
		}
		data = data[start:end]
	}
	return &gwpb.ReadFileResponse{Data: data}, nil
}

func (g *gatewayBridge) ReadDir(ctx context.Context, req *gwpb.ReadDirRequest) (*gwpb.ReadDirResponse, error) {
	dir, err := g.refDir(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, req.DirPath))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "readdir %s: %v", req.DirPath, err)
	}
	var stats []*fstypes.Stat
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		stats = append(stats, &fstypes.Stat{
			Path:    e.Name(),
			Mode:    uint32(info.Mode()),
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
		})
	}
	return &gwpb.ReadDirResponse{Entries: stats}, nil
}

func (g *gatewayBridge) StatFile(ctx context.Context, req *gwpb.StatFileRequest) (*gwpb.StatFileResponse, error) {
	dir, err := g.refDir(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(filepath.Join(dir, req.Path))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "stat %s: %v", req.Path, err)
	}
	return &gwpb.StatFileResponse{Stat: &fstypes.Stat{
		Path:    req.Path,
		Mode:    uint32(info.Mode()),
		Size:    info.Size(),
		ModTime: info.ModTime().UnixNano(),
	}}, nil
}

func (g *gatewayBridge) Evaluate(ctx context.Context, req *gwpb.EvaluateRequest) (*gwpb.EvaluateResponse, error) {
	// Our Solve is eager (it materialises the result), so evaluation is a no-op.
	if _, err := g.refDir(ctx, req.Ref); err != nil {
		return nil, err
	}
	return &gwpb.EvaluateResponse{}, nil
}

func (g *gatewayBridge) Warn(context.Context, *gwpb.WarnRequest) (*gwpb.WarnResponse, error) {
	return &gwpb.WarnResponse{}, nil
}

// Return delivers the build's final result ref (and image-config metadata) to
// the waiting outer Control.Solve.
func (g *gatewayBridge) Return(ctx context.Context, req *gwpb.ReturnRequest) (*gwpb.ReturnResponse, error) {
	// Return must not block-wait or error on an unregistered build: buildx's
	// Internal BuildOpts probe (build/resolver) runs a buildFunc that returns
	// nil, so grpcclient sends a Return for a build we never registered. No-op
	// for it; erroring here fails the probe and aborts the whole build.
	id := buildid.FromIncomingContext(ctx)
	g.cs.gwMu.Lock()
	b := g.cs.gwBuilds[id]
	g.cs.gwMu.Unlock()
	if b == nil {
		return &gwpb.ReturnResponse{}, nil
	}
	if req.Error != nil {
		b.finish("", nil, status.Errorf(codes.Code(req.Error.Code), "%s", req.Error.Message))
		return &gwpb.ReturnResponse{}, nil
	}
	refID := ""
	if req.Result != nil {
		switch r := req.Result.Result.(type) {
		case *gwpb.Result_Ref:
			refID = r.Ref.Id
		case *gwpb.Result_RefDeprecated:
			refID = r.RefDeprecated
		}
	}
	var md map[string][]byte
	if req.Result != nil {
		md = req.Result.Metadata
	}
	b.finish(refID, md, nil)
	return &gwpb.ReturnResponse{}, nil
}
