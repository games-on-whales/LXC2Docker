package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/session"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// recordedBuild captures what Solve forwarded to the build engine so the test
// can assert FrontendAttrs were parsed and the tag/target extracted correctly.
type recordedBuild struct {
	mu        sync.Mutex
	called    bool
	ctxDir    string
	dfPath    string
	tag       string
	target    string
	buildArgs map[string]string
	labels    map[string]string
}

// newSolveTestClient wires a controlServer with stubbed fetch + build so the
// Solve/Status/frontend-parsing logic is exercised over a real gRPC client
// without a session, FileSync, or a root chroot build.
func newSolveTestClient(t *testing.T, rec *recordedBuild, buildErr error) (controlapi.ControlClient, func()) {
	t.Helper()
	sm, err := session.NewManager()
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	cs := &controlServer{
		sm:       sm,
		statuses: map[string]*solveStatus{},
		fetchFn: func(ctx context.Context, sessionID, dfName string) (string, func(), error) {
			return t.TempDir(), func() {}, nil
		},
		buildFn: func(ctx context.Context, ctxDir, dfPath, tag, target string, buildArgs, labels map[string]string, send func(any), getSecret secretFunc) (string, error) {
			rec.mu.Lock()
			rec.called = true
			rec.ctxDir, rec.dfPath, rec.tag, rec.target = ctxDir, dfPath, tag, target
			rec.buildArgs, rec.labels = buildArgs, labels
			rec.mu.Unlock()
			send(map[string]string{"stream": "Step 1: building\n"})
			if buildErr != nil {
				return "", buildErr
			}
			return normalizeImageRef(tag), nil
		},
	}
	srv := grpc.NewServer()
	controlapi.RegisterControlServer(srv, cs)

	h := &Handler{grpcServer: srv}
	mux := http.NewServeMux()
	mux.HandleFunc("/grpc", h.serveGRPC)
	ts := httptest.NewServer(mux)
	addr := strings.TrimPrefix(ts.URL, "http://")

	cc, err := grpc.NewClient(
		"passthrough:///lxc",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(grpcUpgradeDialer(addr)),
	)
	if err != nil {
		ts.Close()
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return controlapi.NewControlClient(cc), func() {
		cc.Close()
		ts.Close()
	}
}

func TestSolveRejectsNonDockerfileFrontend(t *testing.T) {
	client, cleanup := newSolveTestClient(t, &recordedBuild{}, nil)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.Solve(ctx, &controlapi.SolveRequest{
		Ref:      "r1",
		Frontend: "gateway.v0",
	})
	if err == nil || !strings.Contains(err.Error(), "frontend") {
		t.Fatalf("expected frontend rejection, got %v", err)
	}
}

func TestSolveRequiresImageName(t *testing.T) {
	client, cleanup := newSolveTestClient(t, &recordedBuild{}, nil)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.Solve(ctx, &controlapi.SolveRequest{
		Ref:      "r1",
		Frontend: dockerfileFrontend,
	})
	if err == nil || !strings.Contains(err.Error(), "image name") {
		t.Fatalf("expected image-name requirement error, got %v", err)
	}
}

// TestSolveHappyPath drives Solve + a concurrent Status stream and asserts the
// frontend attributes were parsed, the build engine invoked, the image name
// returned, and progress (logs + a completed vertex) streamed to Status.
func TestSolveHappyPath(t *testing.T) {
	rec := &recordedBuild{}
	client, cleanup := newSolveTestClient(t, rec, nil)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Status runs concurrently, as buildx does, collecting progress.
	type statusResult struct {
		logs      strings.Builder
		completed bool
		err       error
	}
	sr := &statusResult{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		stream, err := client.Status(ctx, &controlapi.StatusRequest{Ref: "build-1"})
		if err != nil {
			sr.err = err
			return
		}
		for {
			msg, err := stream.Recv()
			if err != nil {
				return // EOF when Solve closes the channel
			}
			for _, l := range msg.GetLogs() {
				sr.logs.Write(l.GetMsg())
			}
			for _, v := range msg.GetVertexes() {
				if v.GetCompleted() != nil && v.GetError() == "" {
					sr.completed = true
				}
			}
		}
	}()

	// Give Status a moment to attach before Solve produces.
	time.Sleep(100 * time.Millisecond)

	resp, err := client.Solve(ctx, &controlapi.SolveRequest{
		Ref:      "build-1",
		Frontend: dockerfileFrontend,
		FrontendAttrs: map[string]string{
			"filename":          "Dockerfile.web",
			"target":            "final",
			"build-arg:VERSION": "1.2.3",
			"label:owner":       "gow",
		},
		Exporters: []*controlapi.Exporter{
			{Type: "image", Attrs: map[string]string{"name": "example/web:latest"}},
		},
	})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if got := resp.GetExporterResponse()["image.name"]; got != "example/web:latest" {
		t.Fatalf("image.name = %q, want example/web:latest", got)
	}

	wg.Wait()
	if sr.err != nil {
		t.Fatalf("Status: %v", sr.err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.called {
		t.Fatal("build engine was not invoked")
	}
	if rec.dfPath != "Dockerfile.web" {
		t.Errorf("dockerfile = %q, want Dockerfile.web", rec.dfPath)
	}
	if rec.target != "final" {
		t.Errorf("target = %q, want final", rec.target)
	}
	if rec.tag != "example/web:latest" {
		t.Errorf("tag = %q, want example/web:latest", rec.tag)
	}
	if rec.buildArgs["VERSION"] != "1.2.3" {
		t.Errorf("build-arg VERSION = %q, want 1.2.3", rec.buildArgs["VERSION"])
	}
	if rec.labels["owner"] != "gow" {
		t.Errorf("label owner = %q, want gow", rec.labels["owner"])
	}
	if !strings.Contains(sr.logs.String(), "Step 1: building") {
		t.Errorf("status logs missing build output, got %q", sr.logs.String())
	}
	if !sr.completed {
		t.Error("status stream never reported a completed vertex")
	}
}

func TestFrontendBuildArgsAndLabels(t *testing.T) {
	args, labels := frontendBuildArgsAndLabels(map[string]string{
		"build-arg:A": "1",
		"build-arg:B": "two",
		"label:x":     "y",
		"filename":    "Dockerfile",
		"target":      "prod",
	})
	if len(args) != 2 || args["A"] != "1" || args["B"] != "two" {
		t.Fatalf("build args = %v", args)
	}
	if len(labels) != 1 || labels["x"] != "y" {
		t.Fatalf("labels = %v", labels)
	}
}

func TestExporterImageName(t *testing.T) {
	// Prefers the new Exporters list.
	got := exporterImageName(&controlapi.SolveRequest{
		Exporters: []*controlapi.Exporter{{Type: "image", Attrs: map[string]string{"name": "a/b:1,a/b:2"}}},
	})
	if got != "a/b:1" {
		t.Fatalf("first CSV name = %q, want a/b:1", got)
	}
	// Falls back to the deprecated single-exporter attrs.
	got = exporterImageName(&controlapi.SolveRequest{
		ExporterAttrsDeprecated: map[string]string{"name": "legacy:tag"},
	})
	if got != "legacy:tag" {
		t.Fatalf("deprecated name = %q, want legacy:tag", got)
	}
}
