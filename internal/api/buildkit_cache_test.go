package api

import (
	"context"
	"io"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
)

// TestDiskUsageAndPruneSucceed verifies the build-cache RPCs answer cleanly
// (empty) rather than returning Unimplemented, so `docker buildx du` and
// `docker builder prune` work against the daemon.
func TestDiskUsageAndPruneSucceed(t *testing.T) {
	client, cleanup := newSolveTestClient(t, &recordedBuild{}, nil)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	du, err := client.DiskUsage(ctx, &controlapi.DiskUsageRequest{})
	if err != nil {
		t.Fatalf("DiskUsage: %v", err)
	}
	if len(du.GetRecord()) != 0 {
		t.Fatalf("expected no disk-usage records, got %d", len(du.GetRecord()))
	}

	stream, err := client.Prune(ctx, &controlapi.PruneRequest{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Prune recv: %v", err)
		}
	}
}
