package api

import (
	"context"
	"os"
	"path/filepath"

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

// Prune clears the persistent LLB op-output cache (see buildkit_cache_store.go)
// so `docker builder prune` reclaims build-cache disk. It streams no usage
// records — the daemon doesn't track per-entry sizes.
func (s *controlServer) Prune(req *controlapi.PruneRequest, stream grpc.ServerStreamingServer[controlapi.UsageRecord]) error {
	_ = os.RemoveAll(filepath.Join(s.h.cacheDir(), buildCacheSubdir))
	return nil
}
