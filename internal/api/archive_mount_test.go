package api

import (
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// TestResolveArchivePathMounts covers the mount-remap logic that fixes docker
// cp shadowing. All inputs here fall under a mount, so the manager (rootfs
// fallback) is never reached and can be nil.
func TestResolveArchivePathMounts(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddContainer(&store.ContainerRecord{
		ID:   "c1",
		Name: "c1",
		Mounts: []store.MountSpec{
			{Type: "bind", Source: "/host/data", Destination: "/data"},
			{Type: "bind", Source: "/host/data-sub", Destination: "/data/sub"}, // nested, longer
			{Type: "bind", Source: "/host/db", Destination: "/database"},       // prefix-safety vs /data
			{Type: "volume", Source: "/host/vol", Destination: "/ro", ReadOnly: true},
			{Type: "tmpfs", Source: "", Destination: "/tmpfs"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st} // mgr unused: every case below matches a mount

	// A path inside a bind mount resolves to the mount's host source.
	if p, m, err := h.resolveArchivePath("c1", "/data/file.txt"); err != nil || m == nil || p != "/host/data/file.txt" {
		t.Fatalf("/data/file.txt → (%q, %v, %v), want /host/data/file.txt", p, m, err)
	}
	// Nested mount: the LONGEST matching destination wins.
	if p, m, err := h.resolveArchivePath("c1", "/data/sub/x"); err != nil || m == nil || p != "/host/data-sub/x" {
		t.Fatalf("/data/sub/x → (%q, %v, %v), want /host/data-sub/x", p, m, err)
	}
	// The mount point itself maps to the source root.
	if p, _, err := h.resolveArchivePath("c1", "/data"); err != nil || p != "/host/data" {
		t.Fatalf("/data → (%q, %v), want /host/data", p, err)
	}
	// Prefix-safety: /database must NOT be captured by the /data mount.
	if p, m, err := h.resolveArchivePath("c1", "/database/x"); err != nil || m == nil || p != "/host/db/x" {
		t.Fatalf("/database/x → (%q, %v, %v), want /host/db/x (not /host/data)", p, m, err)
	}
	// Read-only mount is reported so the caller can 403 a write.
	if _, m, err := h.resolveArchivePath("c1", "/ro/a"); err != nil || m == nil || !m.ReadOnly {
		t.Fatalf("/ro/a should resolve to the read-only mount, got (%v, %v)", m, err)
	}
	// tmpfs (no host source) errors rather than writing to the shadowed rootfs.
	if _, _, err := h.resolveArchivePath("c1", "/tmpfs/x"); err == nil {
		t.Fatal("/tmpfs/x should error (no host-visible source)")
	}
	// A traversal that escapes the mount subtree cleans to a path outside /data,
	// so it does not resolve against the bind source.
	if p, _, _ := h.resolveArchivePath("c1", "/data/sub/../keep"); p != "/host/data/keep" {
		t.Fatalf("/data/sub/../keep → %q, want /host/data/keep (cleaned, still under /data)", p)
	}
}
