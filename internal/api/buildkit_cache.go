package api

import (
	"context"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
)

// DiskUsage reports BuildKit build-cache usage. This daemon keeps no
// content-addressable build cache — a Solve materialises straight into the
// image store — so there is nothing to report. Answering with an empty record
// set (rather than Unimplemented) lets `docker buildx du` succeed.
func (s *controlServer) DiskUsage(ctx context.Context, req *controlapi.DiskUsageRequest) (*controlapi.DiskUsageResponse, error) {
	return &controlapi.DiskUsageResponse{}, nil
}

// Prune clears the BuildKit build cache. With no cache to clear it streams no
// records, mirroring the classic /build/prune handler so `docker builder prune`
// succeeds (reclaiming nothing) instead of erroring.
func (s *controlServer) Prune(req *controlapi.PruneRequest, stream grpc.ServerStreamingServer[controlapi.UsageRecord]) error {
	return nil
}
