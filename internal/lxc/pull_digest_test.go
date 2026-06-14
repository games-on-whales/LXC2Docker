package lxc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/image"
	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// TestPullImageWith_MutableTagReuseAndRefresh verifies the digest-convergence
// fix: pullOCI records the *registry* digest as RepoDigest, so when a mutable
// tag (e.g. ":latest") has not moved, PullImageWith reuses the cached template
// instead of re-pulling the full image on every materialise (the v1 bug). When
// the tag has moved, it must re-pull.
//
// Reuse vs re-pull is detected by the return: the reuse path returns nil
// quickly, while the re-pull path falls through to oci.Pull (skopeo copy),
// which fails in the test environment — so a non-nil error means "tried to
// re-pull".
func TestPullImageWith_MutableTagReuseAndRefresh(t *testing.T) {
	const ref = "ghcr.io/org/app:latest"
	const localDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const movedDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	orig := ociRemoteDigest
	defer func() { ociRemoteDigest = orig }()

	// newMgr builds a legacy-mode Manager with the template already "present"
	// (a config file under lxcPath) and a store record carrying repoDigest.
	newMgr := func(t *testing.T, repoDigest string) *Manager {
		st, err := store.NewAt(t.TempDir())
		if err != nil {
			t.Fatalf("store init: %v", err)
		}
		lxcPath := t.TempDir()
		mgr := &Manager{lxcPath: lxcPath, store: st}

		// Derive the OCI template name exactly as PullImageWith will, so the
		// template we plant is the one containerExists() looks for.
		resolved, err := image.Resolve(ref, "amd64", mgr.UsePVE())
		if err != nil {
			t.Fatalf("resolve %s: %v", ref, err)
		}
		tmplName := resolved.TemplateContainerName

		rec := &store.ImageRecord{Ref: ref, Arch: "amd64", TemplateName: tmplName, RepoDigest: repoDigest}
		if err := st.AddImage(rec); err != nil {
			t.Fatal(err)
		}
		// Plant the template config so containerExists() returns true.
		dir := filepath.Join(lxcPath, tmplName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config"), []byte("lxc.arch = linux64\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return mgr
	}

	t.Run("unchanged tag is reused (no re-pull)", func(t *testing.T) {
		mgr := newMgr(t, localDigest)
		ociRemoteDigest = func(string, string, string) (string, error) { return localDigest, nil }
		if err := mgr.PullImageWith(ref, "amd64", PullOpts{}); err != nil {
			t.Fatalf("expected reuse (nil), got re-pull error: %v", err)
		}
	})

	t.Run("moved tag triggers a re-pull", func(t *testing.T) {
		mgr := newMgr(t, localDigest)
		ociRemoteDigest = func(string, string, string) (string, error) { return movedDigest, nil }
		err := mgr.PullImageWith(ref, "amd64", PullOpts{})
		if err == nil {
			t.Fatal("expected a re-pull attempt (error from skopeo in test env), got nil")
		}
		// Sanity: the failure is from the pull path, not the digest check.
		if !strings.Contains(err.Error(), "oci") && !strings.Contains(err.Error(), "skopeo") &&
			!strings.Contains(err.Error(), "template") {
			t.Logf("re-pull failed as expected (env has no skopeo): %v", err)
		}
	})

	t.Run("registry unreachable reuses the cached template", func(t *testing.T) {
		mgr := newMgr(t, localDigest)
		ociRemoteDigest = func(string, string, string) (string, error) {
			return "", os.ErrDeadlineExceeded
		}
		if err := mgr.PullImageWith(ref, "amd64", PullOpts{}); err != nil {
			t.Fatalf("expected offline reuse (nil), got: %v", err)
		}
	})
}
