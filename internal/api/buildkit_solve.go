package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/session/grpchijack"
	"github.com/moby/buildkit/session/secrets"
	"github.com/tonistiigi/fsutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// dockerfileFrontend is the only frontend this daemon understands. buildx
// sends it for `docker buildx build` of a Dockerfile; other frontends (e.g.
// gateway.v0 custom frontends) are rejected.
const dockerfileFrontend = "dockerfile.v0"

// solveStatus carries vertex/log updates from an in-flight Solve to the
// matching Status stream. buildx invokes Solve and Status as two concurrent
// RPCs correlated by the build Ref; statusFor hands both sides the same
// channel regardless of which arrives first.
type solveStatus struct {
	ch chan *controlapi.StatusResponse
}

func (s *controlServer) statusFor(ref string) *solveStatus {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	st, ok := s.statuses[ref]
	if !ok {
		st = &solveStatus{ch: make(chan *controlapi.StatusResponse, 256)}
		s.statuses[ref] = st
	}
	return st
}

func (s *controlServer) dropStatus(ref string) {
	s.statusMu.Lock()
	delete(s.statuses, ref)
	s.statusMu.Unlock()
}

// Session bridges buildx's session — FileSync for the build context, plus
// auth/secrets/ssh — into the daemon's session manager. The bidirectional
// stream is hijacked into a net.Conn exactly as upstream buildkit's
// controller does, so the daemon can dial the client's services back.
func (s *controlServer) Session(stream controlapi.Control_SessionServer) error {
	conn, closeCh, opts := grpchijack.Hijack(stream)
	defer conn.Close()

	ctx, cancel := context.WithCancelCause(stream.Context())
	go func() {
		<-closeCh
		cancel(context.Canceled)
	}()
	return s.sm.HandleConn(ctx, conn, opts)
}

// Status streams build progress for req.Ref until the matching Solve finishes
// and closes the channel (or the client disconnects).
func (s *controlServer) Status(req *controlapi.StatusRequest, stream grpc.ServerStreamingServer[controlapi.StatusResponse]) error {
	st := s.statusFor(req.Ref)
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case msg, ok := <-st.ch:
			if !ok {
				s.dropStatus(req.Ref)
				return nil
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// Solve runs a `docker buildx build`. It pulls the Dockerfile and context from
// the client over the session, then drives the shared Dockerfile build engine
// (buildFromContext), streaming progress to the Status RPC as a single vertex
// whose logs carry the build output.
func (s *controlServer) Solve(ctx context.Context, req *controlapi.SolveRequest) (*controlapi.SolveResponse, error) {
	if req.Frontend != dockerfileFrontend {
		return nil, status.Errorf(codes.Unimplemented,
			"docker-lxc-daemon only supports the %q frontend, got %q", dockerfileFrontend, req.Frontend)
	}

	expType, expAttrs := primaryExporter(req)
	tag := firstCSV(expAttrs["name"])
	switch expType {
	case "", "image", "moby":
		if tag == "" {
			return nil, status.Error(codes.InvalidArgument,
				"buildkit build requires an image name; pass -t <name>")
		}
	case "tar", "local":
		// No name required — the result is streamed back to the client.
	default:
		return nil, status.Errorf(codes.Unimplemented,
			"exporter %q is not supported (use the default image output, or --output type=tar|local)", expType)
	}

	st := s.statusFor(req.Ref)
	defer close(st.ch)

	displayName := tag
	if displayName == "" {
		displayName = expType
	}
	vtx := "sha256:" + generateID()
	name := fmt.Sprintf("[lxc] build %s", displayName)
	started := timestamppb.Now()

	emitVertex := func(completed *timestamppb.Timestamp, errMsg string) {
		st.ch <- &controlapi.StatusResponse{Vertexes: []*controlapi.Vertex{{
			Digest:    vtx,
			Name:      name,
			Started:   started,
			Completed: completed,
			Error:     errMsg,
		}}}
	}
	emitLog := func(msg string) {
		st.ch <- &controlapi.StatusResponse{Logs: []*controlapi.VertexLog{{
			Vertex:    vtx,
			Stream:    1, // stdout
			Msg:       []byte(msg),
			Timestamp: timestamppb.Now(),
		}}}
	}
	emitVertex(nil, "") // vertex started

	dfName := req.FrontendAttrs["filename"]
	if dfName == "" {
		dfName = "Dockerfile"
	}
	target := req.FrontendAttrs["target"]
	buildArgs, labels := frontendBuildArgsAndLabels(req.FrontendAttrs)

	ctxDir, cleanup, err := s.fetchFn(ctx, req.Session, dfName)
	if err != nil {
		emitVertex(timestamppb.Now(), err.Error())
		return nil, status.Errorf(codes.Internal, "fetch build context: %v", err)
	}
	defer cleanup()

	send := func(v any) {
		if m, ok := v.(map[string]string); ok {
			if msg, ok := m["stream"]; ok {
				emitLog(msg)
			}
		}
	}

	// getSecret resolves RUN --mount=type=secret values from the client
	// session lazily — sm.Get is only called if the build actually requests a
	// secret, so a build without secrets never needs the session here.
	getSecret := func(sctx context.Context, id string) ([]byte, error) {
		caller, err := s.sm.Get(sctx, req.Session, false)
		if err != nil {
			return nil, err
		}
		return secrets.GetSecret(sctx, caller, id)
	}

	// tar/local exporters stream the built rootfs back to the client instead
	// of importing it into the image store.
	if expType == "tar" || expType == "local" {
		resultDir, rcleanup, _, err := s.h.buildLLBResult(ctx, ctxDir, dfName, target, buildArgs, labels, send, getSecret)
		if err != nil {
			emitVertex(timestamppb.Now(), err.Error())
			return nil, status.Errorf(codes.Internal, "build failed: %v", err)
		}
		defer rcleanup()
		emitLog(fmt.Sprintf("Exporting %s output\n", expType))
		if err := s.exportResultToClient(ctx, req.Session, expType, resultDir); err != nil {
			emitVertex(timestamppb.Now(), err.Error())
			return nil, status.Errorf(codes.Internal, "export %s: %v", expType, err)
		}
		emitVertex(timestamppb.Now(), "")
		return &controlapi.SolveResponse{ExporterResponse: map[string]string{}}, nil
	}

	ref, err := s.buildFn(ctx, ctxDir, dfName, tag, target, buildArgs, labels, send, getSecret)
	if err != nil {
		emitVertex(timestamppb.Now(), err.Error())
		return nil, status.Errorf(codes.Internal, "build failed: %v", err)
	}

	emitLog(fmt.Sprintf("Successfully built %s\n", ref))
	emitVertex(timestamppb.Now(), "") // vertex completed

	return &controlapi.SolveResponse{ExporterResponse: map[string]string{
		"image.name": ref,
	}}, nil
}

// primaryExporter returns the type and attrs of the first requested exporter,
// falling back to the deprecated single-exporter fields.
func primaryExporter(req *controlapi.SolveRequest) (string, map[string]string) {
	for _, e := range req.Exporters {
		return e.GetType(), e.GetAttrs()
	}
	if req.ExporterDeprecated != "" {
		return req.ExporterDeprecated, req.ExporterAttrsDeprecated
	}
	return "", nil
}

// exportResultToClient streams a built result rootfs back to the buildx client
// over the session FileSend: as a directory tree for --output type=local, or
// as a tar stream for --output type=tar.
func (s *controlServer) exportResultToClient(ctx context.Context, sessionID, expType, resultDir string) error {
	caller, err := s.sm.Get(ctx, sessionID, false)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	switch expType {
	case "local":
		fs, err := fsutil.NewFS(resultDir)
		if err != nil {
			return err
		}
		return filesync.CopyToCaller(ctx, fs, 0, caller, nil)
	case "tar":
		w, err := filesync.CopyFileWriter(ctx, nil, 0, caller)
		if err != nil {
			return err
		}
		cmd := exec.Command("tar", "-cf", "-", "-C", resultDir, ".")
		cmd.Stdout = w
		runErr := cmd.Run()
		closeErr := w.Close()
		if runErr != nil {
			return runErr
		}
		return closeErr
	}
	return fmt.Errorf("unsupported export type %q", expType)
}

// fetchBuildContext pulls the build context and Dockerfile from the client
// over the session's FileSync. buildx exposes two local dirs: "context" (the
// build context root) and "dockerfile" (the dir holding the Dockerfile,
// possibly a different path with -f). Both are synced into one directory so
// buildFromContext sees a self-contained context with the Dockerfile in place.
func (s *controlServer) fetchBuildContext(ctx context.Context, sessionID, dfName string) (string, func(), error) {
	caller, err := s.sm.Get(ctx, sessionID, false)
	if err != nil {
		return "", nil, fmt.Errorf("get build session %q: %w", sessionID, err)
	}

	dir, err := os.MkdirTemp("", "buildkit-context-*")
	if err != nil {
		return "", nil, err
	}
	dfDir, err := os.MkdirTemp("", "buildkit-dockerfile-*")
	if err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir); os.RemoveAll(dfDir) }

	// buildx exposes the build context and the Dockerfile as two separate
	// local mounts. They must sync to DISTINCT directories: fsutil mirrors
	// the destination, so syncing "dockerfile" (a single file) into the
	// context dir would delete every other context file. After both syncs,
	// copy the Dockerfile into the context dir so the build sees a
	// self-contained tree (the Dockerfile may already be there if it lived in
	// the context to begin with).
	ctxErr := filesync.FSSync(ctx, caller, filesync.FSSendRequestOpt{Name: "context", DestDir: dir})
	dfErr := filesync.FSSync(ctx, caller, filesync.FSSendRequestOpt{
		Name:            "dockerfile",
		DestDir:         dfDir,
		IncludePatterns: []string{dfName},
	})
	if src := filepath.Join(dfDir, dfName); fileExists(src) {
		if err := copyFile(src, filepath.Join(dir, dfName), 0o644); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("stage Dockerfile: %w", err)
		}
	}

	if _, statErr := os.Stat(filepath.Join(dir, dfName)); statErr != nil {
		cleanup()
		return "", nil, fmt.Errorf("Dockerfile %q not found in build context (context sync: %v; dockerfile sync: %v)", dfName, ctxErr, dfErr)
	}
	return dir, cleanup, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// frontendBuildArgsAndLabels splits the namespaced "build-arg:" and "label:"
// keys buildx packs into FrontendAttrs into plain maps for buildFromContext.
func frontendBuildArgsAndLabels(attrs map[string]string) (buildArgs, labels map[string]string) {
	buildArgs = map[string]string{}
	labels = map[string]string{}
	for k, v := range attrs {
		switch {
		case strings.HasPrefix(k, "build-arg:"):
			buildArgs[strings.TrimPrefix(k, "build-arg:")] = v
		case strings.HasPrefix(k, "label:"):
			labels[strings.TrimPrefix(k, "label:")] = v
		}
	}
	return buildArgs, labels
}

// exporterImageName extracts the target image name (-t) from the requested
// exporters, falling back to the deprecated single-exporter fields. Only the
// first name is used; multi-tag is a Phase 3 concern.
func exporterImageName(req *controlapi.SolveRequest) string {
	pick := func(attrs map[string]string) string {
		if attrs == nil {
			return ""
		}
		return firstCSV(attrs["name"])
	}
	for _, e := range req.Exporters {
		if n := pick(e.Attrs); n != "" {
			return n
		}
	}
	return pick(req.ExporterAttrsDeprecated)
}
