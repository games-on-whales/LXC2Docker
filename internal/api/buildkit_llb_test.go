package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/solver/pb"
	"github.com/opencontainers/go-digest"
)

// fakeMetaResolver returns a minimal but valid OCI image config so LLB
// conversion is hermetic (no skopeo / network).
type fakeMetaResolver struct{}

func (fakeMetaResolver) ResolveImageConfig(ctx context.Context, ref string, _ sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	cfg := []byte(`{"architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/bin"]},"rootfs":{"type":"layers","diff_ids":["sha256:0000000000000000000000000000000000000000000000000000000000000000"]}}`)
	return normalizeImageRef(ref), digest.FromBytes(cfg), cfg, nil
}

// TestDockerfileToLLBProducesOpDAG converts a small Dockerfile and asserts the
// resulting LLB carries the expected operation kinds: an image Source for the
// FROM base, an Exec for RUN, and a File op for COPY.
func TestDockerfileToLLBProducesOpDAG(t *testing.T) {
	df := []byte("FROM busybox\nRUN echo hi > /a\nCOPY x /b\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	def, img, err := dockerfileToLLB(ctx, df, "", map[string]string{}, map[string]string{}, fakeMetaResolver{})
	if err != nil {
		t.Fatalf("dockerfileToLLB: %v", err)
	}
	if img == nil {
		t.Fatal("nil image config")
	}
	if len(def.Def) == 0 {
		t.Fatal("empty LLB definition")
	}

	verts, _, err := llbOps(def)
	if err != nil {
		t.Fatalf("llbOps: %v", err)
	}

	var sawImageSource, sawExec, sawFile, sawLocalContext bool
	var sourceIDs []string
	for _, v := range verts {
		switch op := v.op.GetOp().(type) {
		case *pb.Op_Source:
			id := op.Source.GetIdentifier()
			sourceIDs = append(sourceIDs, id)
			if strings.HasPrefix(id, "docker-image://") {
				sawImageSource = true
			}
			if strings.HasPrefix(id, "local://context") {
				sawLocalContext = true
			}
		case *pb.Op_Exec:
			sawExec = true
		case *pb.Op_File:
			sawFile = true
		}
	}
	t.Logf("source identifiers: %v", sourceIDs)
	if !sawImageSource {
		t.Errorf("no docker-image Source op for FROM busybox; sources=%v", sourceIDs)
	}
	if !sawExec {
		t.Error("no Exec op for RUN")
	}
	if !sawFile {
		t.Error("no File op for COPY")
	}
	if !sawLocalContext {
		t.Error("no local://context Source op for the build context (COPY source)")
	}
}

// recordMetaResolver mimics skopeoMetaResolver's local-store fast path: it
// returns the config produced by imageRecordToOCIImage for a stored image. This
// is the exact path a build takes when the base image is already pulled.
type recordMetaResolver struct{ rec *store.ImageRecord }

func (r recordMetaResolver) ResolveImageConfig(ctx context.Context, ref string, _ sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	cfg, err := json.Marshal(imageRecordToOCIImage(r.rec))
	if err != nil {
		return "", "", nil, err
	}
	return normalizeImageRef(ref), digest.FromBytes(cfg), cfg, nil
}

// TestImageRecordResolverKeepsBaseImage guards against the scratch-base
// regression: dockerfile2llb treats a base config with no RootFS.DiffIDs as
// "scratch" and silently drops the FROM base, so every RUN executes on an empty
// rootfs (no /bin/sh, no dynamic linker → the build CT fails to exec). The
// local-store fast path must therefore produce a config with diff IDs so the
// FROM base survives as a real docker-image Source op.
func TestImageRecordResolverKeepsBaseImage(t *testing.T) {
	rec := &store.ImageRecord{ID: "oci_debian_bookworm", Ref: "debian:bookworm"}

	// The config builder itself must carry at least one diff ID.
	img := imageRecordToOCIImage(rec)
	if len(img.RootFS.DiffIDs) == 0 {
		t.Fatal("imageRecordToOCIImage produced no RootFS.DiffIDs; dockerfile2llb will treat the base as scratch")
	}

	def, _, err := dockerfileToLLB(context.Background(), []byte("FROM debian:bookworm\nRUN echo hi > /a\n"), "", map[string]string{}, map[string]string{}, recordMetaResolver{rec: rec})
	if err != nil {
		t.Fatalf("dockerfileToLLB: %v", err)
	}
	verts, _, err := llbOps(def)
	if err != nil {
		t.Fatalf("llbOps: %v", err)
	}
	var sawImageSource bool
	for _, v := range verts {
		if op, ok := v.op.GetOp().(*pb.Op_Source); ok && strings.HasPrefix(op.Source.GetIdentifier(), "docker-image://") {
			sawImageSource = true
		}
	}
	if !sawImageSource {
		t.Error("FROM base collapsed to scratch: no docker-image Source op (RUN would run on an empty rootfs)")
	}
}
