package api

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// grpcUpgradeDialer performs the same /grpc handshake buildx's docker driver
// does: POST /grpc with Connection: Upgrade / Upgrade: h2c, read the 101, then
// hand the raw connection to gRPC (which speaks HTTP/2 on it directly).
func grpcUpgradeDialer(addr string) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		reqLine := "POST /grpc HTTP/1.1\r\nHost: lxc\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n"
		if _, err := conn.Write([]byte(reqLine)); err != nil {
			conn.Close()
			return nil, err
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
		if err != nil {
			conn.Close()
			return nil, err
		}
		if resp.StatusCode != http.StatusSwitchingProtocols {
			conn.Close()
			return nil, fmt.Errorf("expected 101 Switching Protocols, got %d", resp.StatusCode)
		}
		return conn, nil
	}
}

func newBuildkitTestClient(t *testing.T) (controlapi.ControlClient, func()) {
	t.Helper()
	h := &Handler{}
	h.grpcServer = newBuildkitControlServer(h)
	mux := http.NewServeMux()
	mux.HandleFunc("/grpc", h.serveGRPC)
	srv := httptest.NewServer(mux)
	addr := strings.TrimPrefix(srv.URL, "http://")

	cc, err := grpc.NewClient(
		"passthrough:///lxc",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(grpcUpgradeDialer(addr)),
	)
	if err != nil {
		srv.Close()
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return controlapi.NewControlClient(cc), func() {
		cc.Close()
		srv.Close()
	}
}

// TestBuildkitControlInfo verifies the /grpc endpoint completes the h2c upgrade
// and answers Control.Info — the call buildx makes first to decide the docker
// driver is BuildKit-capable.
func TestBuildkitControlInfo(t *testing.T) {
	client, cleanup := newBuildkitTestClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.Info(ctx, &controlapi.InfoRequest{})
	if err != nil {
		t.Fatalf("Info RPC: %v", err)
	}
	if resp.GetBuildkitVersion() == nil {
		t.Fatal("Info returned no BuildkitVersion")
	}
	if got := resp.GetBuildkitVersion().GetVersion(); got != buildkitVersion {
		t.Fatalf("BuildkitVersion.Version = %q, want %q", got, buildkitVersion)
	}
}

// TestBuildkitControlListWorkers verifies the daemon advertises exactly one
// linux/amd64 worker, which buildx validates --platform requests against.
func TestBuildkitControlListWorkers(t *testing.T) {
	client, cleanup := newBuildkitTestClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.ListWorkers(ctx, &controlapi.ListWorkersRequest{})
	if err != nil {
		t.Fatalf("ListWorkers RPC: %v", err)
	}
	if len(resp.GetRecord()) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(resp.GetRecord()))
	}
	w := resp.GetRecord()[0]
	if len(w.GetPlatforms()) == 0 {
		t.Fatal("worker advertises no platforms")
	}
	p := w.GetPlatforms()[0]
	if p.GetOS() != "linux" || p.GetArchitecture() != "amd64" {
		t.Fatalf("worker platform = %s/%s, want linux/amd64", p.GetOS(), p.GetArchitecture())
	}
}
