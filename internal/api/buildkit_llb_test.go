package api

import (
	"context"
	"strings"
	"testing"

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
