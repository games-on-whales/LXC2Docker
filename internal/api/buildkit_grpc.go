package api

import (
	"bufio"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"

	controlapi "github.com/moby/buildkit/api/services/control"
	bktypes "github.com/moby/buildkit/api/types"
	gwpb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/session"
	pb "github.com/moby/buildkit/solver/pb"
	"google.golang.org/grpc"
)

// buildkitVersion is the buildkit release whose protobuf/gRPC API this daemon
// speaks. Reported to buildx so its docker driver negotiates against us as a
// genuine BuildKit-capable engine rather than falling back or erroring.
const buildkitVersion = "v0.30.0"

// newBuildkitControlServer constructs the gRPC server that exposes the
// moby.buildkit.v1.Control service. buildx's "docker" driver dials the
// engine's /grpc endpoint and drives builds entirely over this service —
// Info/ListWorkers for negotiation, Solve/Status/Session for the build.
func newBuildkitControlServer(h *Handler) *grpc.Server {
	// session.NewManager only allocates in-memory maps, so the error is
	// effectively unreachable; the daemon can't build without it, so treat a
	// failure as fatal at construction.
	sm, err := session.NewManager()
	if err != nil {
		panic("buildkit session manager: " + err.Error())
	}
	cs := &controlServer{
		h:        h,
		sm:       sm,
		statuses: map[string]*solveStatus{},
		gwBuilds: map[string]*gatewayBuild{},
	}
	// Share the session manager with the HTTP layer so the /session endpoint
	// (buildx's docker driver opens it for build-context FileSync) registers
	// connections into the same manager that Solve reads from.
	h.buildkitSM = sm
	// buildx Solve uses the real LLB executor; the classic POST /build handler
	// keeps using buildFromContext directly.
	cs.buildFn = h.buildViaLLB
	cs.fetchFn = cs.fetchBuildContext
	srv := grpc.NewServer()
	controlapi.RegisterControlServer(srv, cs)
	// buildx's docker driver drives the Dockerfile frontend client-side and
	// calls the LLBBridge service back over this same connection. A separate
	// type is required because both services declare a Solve method.
	gwpb.RegisterLLBBridgeServer(srv, &gatewayBridge{cs: cs})
	return srv
}

// serveSession handles buildx's POST /session. buildx's docker driver opens a
// long-lived session connection here (separate from the /grpc Control channel)
// and serves its FileSync/auth/secrets attachables over it; Solve later dials
// back through the session manager to pull the build context. The buildkit
// session manager owns the HTTP hijack + h2c upgrade, so we just hand it the
// request — mirroring how the real moby daemon wires its /session route.
func (h *Handler) serveSession(w http.ResponseWriter, r *http.Request) {
	if h.buildkitSM == nil {
		errResponse(w, http.StatusNotImplemented, "buildkit session manager unavailable")
		return
	}
	if err := h.buildkitSM.HandleHTTPRequest(r.Context(), w, r); err != nil {
		// The connection is already hijacked by the time most errors occur, so
		// an HTTP error response is no longer possible — log for diagnosis.
		log.Printf("buildkit session: %v", err)
	}
}

// controlServer implements moby.buildkit.v1.Control on top of the LXC daemon.
// Embedding UnimplementedControlServer (by value, per its contract) keeps us
// forward-compatible as buildkit adds RPCs; only the methods we implement here
// are live, the rest return codes.Unimplemented.
type controlServer struct {
	controlapi.UnimplementedControlServer
	h  *Handler
	sm *session.Manager

	// statuses correlates an in-flight Solve with its concurrent Status
	// stream by build Ref. See buildkit_solve.go.
	statusMu sync.Mutex
	statuses map[string]*solveStatus

	// gwBuilds holds in-flight gateway-mode builds (buildx's docker driver),
	// keyed by build ref. The LLBBridge service (buildkit_gateway.go) drives
	// them; the outer Control.Solve waits on Return.
	gwMu     sync.Mutex
	gwBuilds map[string]*gatewayBuild

	// buildFn runs the actual Dockerfile build once the context is fetched.
	// It defaults to buildViaLLB; tests override it to exercise the Solve
	// plumbing without a real build. getSecret resolves RUN --mount=secret
	// values from the client session.
	buildFn func(ctx context.Context, ctxDir, dockerfilePath, tag, targetStage string, buildArgs, queryLabels map[string]string, send func(any), getSecret secretFunc) (string, error)

	// fetchFn pulls the build context + Dockerfile from the client session.
	// Defaults to fetchBuildContext; tests override it to bypass FileSync.
	fetchFn func(ctx context.Context, sessionID, dfName string) (string, func(), error)
}

func lxcBuildkitVersion() *bktypes.BuildkitVersion {
	return &bktypes.BuildkitVersion{
		Package:  "github.com/games-on-whales/LXC2Docker",
		Version:  buildkitVersion,
		Revision: "lxc",
	}
}

// Info reports the BuildKit version. buildx calls this during bootstrap to
// decide the driver is usable and to pick a compatible dockerfile frontend.
func (s *controlServer) Info(ctx context.Context, req *controlapi.InfoRequest) (*controlapi.InfoResponse, error) {
	return &controlapi.InfoResponse{BuildkitVersion: lxcBuildkitVersion()}, nil
}

// ListWorkers advertises a single LXC-backed worker for linux/amd64. buildx
// uses the platform set to validate --platform requests against the driver.
func (s *controlServer) ListWorkers(ctx context.Context, req *controlapi.ListWorkersRequest) (*controlapi.ListWorkersResponse, error) {
	return &controlapi.ListWorkersResponse{
		Record: []*bktypes.WorkerRecord{{
			ID: "lxc",
			Labels: map[string]string{
				"org.mobyproject.buildkit.worker.executor":          "lxc",
				"org.mobyproject.buildkit.worker.moby.host-gateway": "",
			},
			Platforms:       []*pb.Platform{{Architecture: "amd64", OS: "linux"}},
			BuildkitVersion: lxcBuildkitVersion(),
		}},
	}, nil
}

// serveGRPC hijacks the HTTP connection and hands it to the BuildKit gRPC
// server. The wire shape mirrors the real moby daemon's /grpc route: respond
// 101 with Upgrade: h2c, then speak gRPC (which carries its own HTTP/2
// framing) directly on the raw connection.
func (h *Handler) serveGRPC(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		errResponse(w, http.StatusInternalServerError, "grpc requires a hijackable connection")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}

	if _, err := buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n"); err != nil {
		conn.Close()
		return
	}
	if err := buf.Flush(); err != nil {
		conn.Close()
		return
	}

	// Reads must drain any bytes the HTTP server already buffered (the start
	// of the client's HTTP/2 preface) before falling through to the socket;
	// buf.Reader does exactly that. Writes go straight to conn since the
	// 101 response is already flushed.
	served := &hijackedConn{Conn: conn, r: buf.Reader}
	// Serve blocks until the connection is closed by either side; the
	// single-conn listener returns the conn once, then unblocks Accept when
	// the gRPC transport closes it, letting Serve return cleanly.
	_ = h.grpcServer.Serve(newSingleConnListener(served))
}

// hijackedConn overrides Read to consume buffered bytes first while delegating
// everything else to the underlying connection.
type hijackedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *hijackedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// singleConnListener adapts one already-accepted connection into a
// net.Listener so it can be handed to grpc.Server.Serve. Accept yields the
// connection exactly once; the second Accept blocks until that connection is
// closed and then returns an error, which is how grpc.Server.Serve learns to
// stop.
type singleConnListener struct {
	mu     sync.Mutex
	conn   net.Conn
	closed chan struct{}
}

var errListenerDrained = errors.New("buildkit grpc listener drained")

func newSingleConnListener(conn net.Conn) *singleConnListener {
	l := &singleConnListener{closed: make(chan struct{})}
	l.conn = &listenerConn{Conn: conn, l: l}
	return l
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, errListenerDrained
}

func (l *singleConnListener) Close() error { return nil }

func (l *singleConnListener) Addr() net.Addr { return buildkitAddr{} }

// listenerConn signals the listener when the served connection is closed so a
// blocked second Accept can return and Serve can exit.
type listenerConn struct {
	net.Conn
	l    *singleConnListener
	once sync.Once
}

func (c *listenerConn) Close() error {
	c.once.Do(func() { close(c.l.closed) })
	return c.Conn.Close()
}

type buildkitAddr struct{}

func (buildkitAddr) Network() string { return "unix" }
func (buildkitAddr) String() string  { return "buildkit-grpc" }
