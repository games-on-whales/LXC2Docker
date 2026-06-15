package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/games-on-whales/LXC2Docker/internal/store"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	"github.com/moby/buildkit/frontend/dockerui"
	"github.com/moby/buildkit/solver/pb"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/protobuf/proto"
)

// skopeoMetaResolver resolves a base image reference to its OCI config so the
// Dockerfile→LLB conversion knows the base image's env/entrypoint/platform. It
// shells out to `skopeo inspect --config`, matching how the rest of the daemon
// talks to registries (internal/oci). It only fetches metadata — the base
// rootfs is materialised later by the executor's Source op.
type skopeoMetaResolver struct{ h *Handler }

func (r skopeoMetaResolver) ResolveImageConfig(ctx context.Context, ref string, _ sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	normalized := normalizeImageRef(ref)
	// Prefer the local image store: ensure the base is pulled, then reconstruct
	// its config from the record. This avoids a separate live-registry round-trip
	// (skopeo inspect) per FROM — which is slow and rate-limited — and reuses the
	// same image the executor will mount.
	if r.h != nil {
		// Already-pulled image: use its stored config directly.
		if rec := r.h.store.GetImage(normalized); rec != nil {
			if cfg, mErr := json.Marshal(imageRecordToOCIImage(rec)); mErr == nil {
				return normalized, digest.FromBytes(cfg), cfg, nil
			}
		}
		// Not local yet: pull it, then use the stored config.
		if err := r.h.ensureBuildBaseImage(normalized, func(any) {}); err == nil {
			if rec := r.h.store.GetImage(normalized); rec != nil {
				if cfg, mErr := json.Marshal(imageRecordToOCIImage(rec)); mErr == nil {
					return normalized, digest.FromBytes(cfg), cfg, nil
				}
			}
		}
	}
	out, err := exec.CommandContext(ctx, "skopeo", "inspect", "--config", "docker://"+normalized).Output()
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve base image %s: %w", normalized, err)
	}
	return normalized, digest.FromBytes(out), out, nil
}

// imageRecordToOCIImage reconstructs an OCI image config from a stored image
// record (env/cmd/entrypoint/etc.), for FROM resolution and build export.
//
// RootFS.DiffIDs must be non-empty: dockerfile2llb treats a base image config
// with zero diff IDs as "scratch" (convert.go: len(img.RootFS.DiffIDs)==0 →
// llb.Scratch()), which silently drops the FROM base so every RUN executes on
// an empty rootfs. Our executor materialises the base by ref (not by diff ID),
// so a single synthetic diff ID keeps the base a real image while staying
// deterministic across builds (important for LLB cache keys). It is derived
// from the image's content identity (RepoDigest) so that re-pulling a moving
// tag with new content changes the diff ID — and thus the base Source op and
// every downstream RUN's cache key — instead of serving a stale cached result.
// It falls back to the record ID when no digest is recorded yet.
func imageRecordToOCIImage(rec *store.ImageRecord) dockerspec.DockerOCIImage {
	baseDiffID := digest.FromString("dld-base:" + rec.ID + ":" + rec.RepoDigest)
	return dockerspec.DockerOCIImage{
		Image: ocispecs.Image{
			Platform: ocispecs.Platform{Architecture: "amd64", OS: "linux"},
			RootFS: ocispecs.RootFS{
				Type:    "layers",
				DiffIDs: []digest.Digest{baseDiffID},
			},
		},
		// DockerOCIImage shadows the embedded ocispecs.Image.Config with its own
		// Config field (json:"config"), so the config MUST be set here — setting
		// the embedded ocispecs.Image.Config instead marshals an empty "config",
		// dropping the base image's Env (HOME, PATH, ...). dockerfile2llb then sees
		// a base with no environment and falls back to a default PATH, breaking
		// `ENV PATH="$HOME/.cargo/bin:$PATH"`-style instructions during the build.
		Config: dockerspec.DockerOCIImageConfig{
			ImageConfig: ocispecs.ImageConfig{
				Env:        rec.OCIEnv,
				Cmd:        rec.OCICmd,
				Entrypoint: rec.OCIEntrypoint,
				WorkingDir: rec.OCIWorkingDir,
				User:       rec.OCIUser,
				Labels:     rec.OCILabels,
			},
		},
	}
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
