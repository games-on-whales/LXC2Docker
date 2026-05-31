package api

import (
	"testing"

	"github.com/moby/buildkit/solver/pb"
	"github.com/opencontainers/go-digest"
)

// TestIsCacheable verifies the conservative cacheability rule: ops are
// cacheable only when their whole ancestry is digest-pinned docker-image
// sources; anything reaching a local://context source is never cached (so the
// op digest remains a sound content key and stale results are impossible).
func TestIsCacheable(t *testing.T) {
	dImg := digest.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")
	dLocal := digest.Digest("sha256:2222222222222222222222222222222222222222222222222222222222222222")
	dRunOnImg := digest.Digest("sha256:3333333333333333333333333333333333333333333333333333333333333333")
	dCopyFromLocal := digest.Digest("sha256:4444444444444444444444444444444444444444444444444444444444444444")

	byDigest := map[digest.Digest]*pb.Op{
		dImg:   {Op: &pb.Op_Source{Source: &pb.SourceOp{Identifier: "docker-image://docker.io/library/busybox:latest@sha256:abc"}}},
		dLocal: {Op: &pb.Op_Source{Source: &pb.SourceOp{Identifier: "local://context"}}},
		dRunOnImg: {
			Inputs: []*pb.Input{{Digest: string(dImg), Index: 0}},
			Op:     &pb.Op_Exec{Exec: &pb.ExecOp{Meta: &pb.Meta{Args: []string{"/bin/sh", "-c", "echo hi"}}}},
		},
		dCopyFromLocal: {
			Inputs: []*pb.Input{{Digest: string(dImg), Index: 0}, {Digest: string(dLocal), Index: 0}},
			Op:     &pb.Op_File{File: &pb.FileOp{}},
		},
	}

	e := &llbExecutor{byDigest: byDigest, cacheableMemo: map[digest.Digest]bool{}}

	if !e.isCacheable(dImg) {
		t.Error("docker-image source should be cacheable")
	}
	if e.isCacheable(dLocal) {
		t.Error("local://context source must NOT be cacheable")
	}
	if !e.isCacheable(dRunOnImg) {
		t.Error("RUN on a pinned image should be cacheable")
	}
	if e.isCacheable(dCopyFromLocal) {
		t.Error("an op consuming local context must NOT be cacheable")
	}
}
