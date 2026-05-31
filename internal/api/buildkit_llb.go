package api

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	"github.com/moby/buildkit/frontend/dockerui"
	"github.com/moby/buildkit/solver/pb"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/opencontainers/go-digest"
	"google.golang.org/protobuf/proto"
)

// skopeoMetaResolver resolves a base image reference to its OCI config so the
// Dockerfile→LLB conversion knows the base image's env/entrypoint/platform. It
// shells out to `skopeo inspect --config`, matching how the rest of the daemon
// talks to registries (internal/oci). It only fetches metadata — the base
// rootfs is materialised later by the executor's Source op.
type skopeoMetaResolver struct{}

func (skopeoMetaResolver) ResolveImageConfig(ctx context.Context, ref string, _ sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	normalized := normalizeImageRef(ref)
	out, err := exec.CommandContext(ctx, "skopeo", "inspect", "--config", "docker://"+normalized).Output()
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve base image %s: %w", normalized, err)
	}
	return normalized, digest.FromBytes(out), out, nil
}

// dockerfileToLLB converts Dockerfile bytes into a real BuildKit LLB definition
// plus the resulting image config. The build context is left as the implicit
// llb.Local("context") source (Client is nil), which the executor resolves to
// the FileSync'd context directory. The returned *llb.Definition carries the
// marshalled pb.Op DAG in Def; use llbOps to decode it.
func dockerfileToLLB(ctx context.Context, dfBytes []byte, target string, buildArgs, labels map[string]string, resolver llb.ImageMetaResolver) (*llb.Definition, *dockerspec.DockerOCIImage, error) {
	res, err := dockerfile2llb.Dockerfile2LLB(ctx, dfBytes, dockerfile2llb.ConvertOpt{
		Config: dockerui.Config{
			BuildArgs: buildArgs,
			Labels:    labels,
			Target:    target,
		},
		MetaResolver: resolver,
	})
	if err != nil {
		return nil, nil, err
	}
	def, err := res.State.Marshal(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal LLB: %w", err)
	}
	return def, res.Image, nil
}

// llbVertex is one decoded node of the LLB DAG: the operation plus the digest
// other ops reference it by (the digest of its marshalled bytes).
type llbVertex struct {
	digest digest.Digest
	op     *pb.Op
}

// llbOps decodes a marshalled LLB definition into its operations keyed by the
// digest used in Op.Inputs edges, preserving definition order. The final entry
// in def.Def is the synthetic result op whose single input points at the build
// result; callers walk inputs from there.
func llbOps(def *llb.Definition) ([]llbVertex, map[digest.Digest]*pb.Op, error) {
	out := make([]llbVertex, 0, len(def.Def))
	byDigest := make(map[digest.Digest]*pb.Op, len(def.Def))
	for _, dt := range def.Def {
		op := &pb.Op{}
		if err := proto.Unmarshal(dt, op); err != nil {
			return nil, nil, fmt.Errorf("decode LLB op: %w", err)
		}
		dgst := digest.FromBytes(dt)
		out = append(out, llbVertex{digest: dgst, op: op})
		byDigest[dgst] = op
	}
	return out, byDigest, nil
}
