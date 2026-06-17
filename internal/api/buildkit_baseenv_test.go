package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/opencontainers/go-digest"
)

// imageRecordToOCIImage must place the config under DockerOCIImage.Config (which
// shadows the embedded ocispec.Image.Config in JSON). Setting the embedded field
// instead marshals an empty "config", silently dropping the base image's Env —
// which made every `docker build` lose HOME/PATH from its base.
func TestImageRecordToOCIImage_MarshalsEnvIntoConfig(t *testing.T) {
	rec := &store.ImageRecord{ID: "base", OCIEnv: []string{"HOME=/home/retro", "PATH=/base/bin"}}

	raw, err := json.Marshal(imageRecordToOCIImage(rec))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Round-trip exactly as dockerfile2llb does.
	var img dockerspec.DockerOCIImage
	if err := json.Unmarshal(raw, &img); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := envValue(img.Config.Env, "HOME"); got != "/home/retro" {
		t.Errorf("base env not marshaled under config: HOME=%q, full=%v (raw=%s)", got, img.Config.Env, raw)
	}
}

// envMetaResolver returns a base image config built the same way the daemon does
// (via imageRecordToOCIImage), so the test exercises the real config marshaling.
type envMetaResolver struct{ env []string }

func (r envMetaResolver) ResolveImageConfig(_ context.Context, ref string, _ sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	cfg, err := json.Marshal(imageRecordToOCIImage(&store.ImageRecord{ID: ref, OCIEnv: r.env}))
	if err != nil {
		return "", "", nil, err
	}
	return ref, digest.FromBytes(cfg), cfg, nil
}

// End-to-end: a Dockerfile FROM a base with HOME set must carry that HOME into
// the converted image config, so `ENV PATH="$HOME/..."` expands correctly during
// the build.
func TestDockerfileToLLB_PropagatesBaseEnv(t *testing.T) {
	df := []byte("FROM example.com/base:latest\nENV EXTRA=1\nRUN true\n")
	resolver := envMetaResolver{env: []string{"HOME=/home/retro", "PATH=/base/bin", "LANG=C.UTF-8"}}

	_, img, err := dockerfileToLLB(context.Background(), df, "", nil, nil, resolver)
	if err != nil {
		t.Fatalf("dockerfileToLLB: %v", err)
	}
	if img == nil {
		t.Fatal("nil image config")
	}
	t.Logf("resulting env: %v", img.Config.Env)
	if !strings.Contains(strings.Join(img.Config.Env, "\n"), "HOME=/home/retro") {
		t.Errorf("base HOME not propagated into built image config; env=%v", img.Config.Env)
	}
}
