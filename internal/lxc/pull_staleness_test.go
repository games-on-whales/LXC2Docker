package lxc

import (
	"errors"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/image"
	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// TestOCITagMoved exercises the staleness check that lets PullImageWith refresh
// a re-pushed mutable tag (e.g. ":latest") instead of serving a frozen local
// template. The registry round-trip is stubbed via the ociRemoteDigest seam.
func TestOCITagMoved(t *testing.T) {
	const ref = "ghcr.io/org/app:latest"
	const localDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const movedDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	// Swap the registry lookup for the duration of the test.
	orig := ociRemoteDigest
	defer func() { ociRemoteDigest = orig }()

	newMgr := func(t *testing.T) *Manager {
		st, err := store.NewAt(t.TempDir())
		if err != nil {
			t.Fatalf("store init: %v", err)
		}
		return &Manager{lxcPath: t.TempDir(), store: st}
	}
	ociImage := &image.ResolvedImage{Ref: ref, Kind: image.KindOCI}

	t.Run("tag moved in registry → true", func(t *testing.T) {
		mgr := newMgr(t)
		if err := mgr.store.AddImage(&store.ImageRecord{Ref: ref, RepoDigest: localDigest}); err != nil {
			t.Fatal(err)
		}
		ociRemoteDigest = func(string, string) (string, error) { return movedDigest, nil }
		if !mgr.ociTagMoved(ociImage, PullOpts{}) {
			t.Error("expected ociTagMoved=true when registry digest differs")
		}
	})

	t.Run("digest unchanged → false", func(t *testing.T) {
		mgr := newMgr(t)
		if err := mgr.store.AddImage(&store.ImageRecord{Ref: ref, RepoDigest: localDigest}); err != nil {
			t.Fatal(err)
		}
		ociRemoteDigest = func(string, string) (string, error) { return localDigest, nil }
		if mgr.ociTagMoved(ociImage, PullOpts{}) {
			t.Error("expected ociTagMoved=false when registry digest matches")
		}
	})

	t.Run("registry unreachable → false (offline-safe)", func(t *testing.T) {
		mgr := newMgr(t)
		if err := mgr.store.AddImage(&store.ImageRecord{Ref: ref, RepoDigest: localDigest}); err != nil {
			t.Fatal(err)
		}
		ociRemoteDigest = func(string, string) (string, error) { return "", errors.New("network down") }
		if mgr.ociTagMoved(ociImage, PullOpts{}) {
			t.Error("expected ociTagMoved=false when the registry cannot be reached")
		}
	})

	t.Run("digest-pinned ref → false (immutable, never re-checked)", func(t *testing.T) {
		mgr := newMgr(t)
		pinned := "ghcr.io/org/app@" + localDigest
		if err := mgr.store.AddImage(&store.ImageRecord{Ref: pinned, RepoDigest: localDigest}); err != nil {
			t.Fatal(err)
		}
		called := false
		ociRemoteDigest = func(string, string) (string, error) { called = true; return movedDigest, nil }
		if mgr.ociTagMoved(&image.ResolvedImage{Ref: pinned, Kind: image.KindOCI}, PullOpts{}) {
			t.Error("expected ociTagMoved=false for a digest-pinned ref")
		}
		if called {
			t.Error("registry should not be queried for a digest-pinned ref")
		}
	})

	t.Run("no local digest recorded → false", func(t *testing.T) {
		mgr := newMgr(t)
		if err := mgr.store.AddImage(&store.ImageRecord{Ref: ref}); err != nil { // RepoDigest empty
			t.Fatal(err)
		}
		ociRemoteDigest = func(string, string) (string, error) { return movedDigest, nil }
		if mgr.ociTagMoved(ociImage, PullOpts{}) {
			t.Error("expected ociTagMoved=false when no local digest is recorded")
		}
	})

	t.Run("non-OCI image → false", func(t *testing.T) {
		mgr := newMgr(t)
		distro := &image.ResolvedImage{Ref: "ubuntu:22.04", Kind: image.KindDistro}
		if mgr.ociTagMoved(distro, PullOpts{}) {
			t.Error("expected ociTagMoved=false for a non-OCI image")
		}
	})
}
