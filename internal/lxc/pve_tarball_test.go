package lxc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestResolveRootfsGB(t *testing.T) {
	// A small tarball file (<1G) yields the 4G image minimum.
	tarball := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	if err := os.WriteFile(tarball, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if min := tarballRootfsGB(tarball); min != 4 {
		t.Fatalf("precondition: tarballRootfsGB = %d, want 4", min)
	}

	cases := []struct {
		name   string
		diskGB int
		want   int
	}{
		{"unset uses image minimum", 0, 4},
		{"below minimum is bumped", 2, 4},
		{"at minimum honored", 4, 4},
		{"above minimum honored", 64, 64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveRootfsGB(ContainerConfig{DiskSizeGB: tc.diskGB}, tarball)
			if got != tc.want {
				t.Fatalf("resolveRootfsGB(DiskSizeGB=%d) = %d, want %d", tc.diskGB, got, tc.want)
			}
		})
	}
}

func TestSanitizeDatasetToken(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"ubuntu_22.04", "ubuntu_22.04"},
		{"ghcr.io/games-on-whales/steam:edge", "ghcr.io-games-on-whales-steam-edge"},
		{"sha256:abcDEF123", "sha256-abcDEF123"},
		{"--leading-and-trailing--", "leading-and-trailing"},
		{"", "img"},
		{"!!!", "img"},
	}
	for _, tc := range cases {
		if got := sanitizeDatasetToken(tc.in); got != tc.want {
			t.Errorf("sanitizeDatasetToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPVEStorageSupportsLinkedClone(t *testing.T) {
	cases := []struct {
		stgType string
		want    bool
	}{
		{"lvmthin", true},
		{"zfspool", false}, // ZFS uses its own raw-clone fast path
		{"dir", false},
		{"lvm", false},
		{"nfs", false},
		{"", false},
	}
	for _, tc := range cases {
		m := &Manager{pveStgType: tc.stgType}
		if got := m.pveStorageSupportsLinkedClone(); got != tc.want {
			t.Errorf("pveStorageSupportsLinkedClone(%q) = %v, want %v", tc.stgType, got, tc.want)
		}
	}
}

func TestImageTemplateCTHostname(t *testing.T) {
	cases := []struct {
		id, want string
	}{
		{"ubuntu_22.04", "dld-tmpl-ubuntu-22-04"},
		{"ghcr.io/games-on-whales/steam:edge", "dld-tmpl-ghcr-io-games-on-whales-steam-edge"},
	}
	for _, tc := range cases {
		if got := imageTemplateCTHostname(&store.ImageRecord{ID: tc.id}); got != tc.want {
			t.Errorf("imageTemplateCTHostname(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestImageTemplateDataset(t *testing.T) {
	m := &Manager{pveStorage: "tank"}
	got := m.imageTemplateDataset(&store.ImageRecord{ID: "ubuntu_22.04"})
	if want := "tank/dld-tmpl-ubuntu_22.04"; got != want {
		t.Fatalf("imageTemplateDataset = %q, want %q", got, want)
	}
}

func TestReapOrphanTarballs(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	mgr := &Manager{store: st, pveStorage: "storage"}

	dir := filepath.Join(st.RootDir(), "pve-templates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, aged bool) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if aged {
			old := time.Now().Add(-2 * orphanCTGrace)
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}

	// referenced: an image record points at it → must survive.
	referenced := write("referenced.tar.gz", true)
	if err := st.AddImage(&store.ImageRecord{ID: "i1", Ref: "busybox", TemplateTarball: referenced}); err != nil {
		t.Fatal(err)
	}
	// orphan, aged past the grace → must be reaped.
	orphan := write("orphan.tar.gz", true)
	// orphan but freshly written (mid-pull) → must survive the grace window.
	fresh := write("fresh.tar.gz", false)

	mgr.reapOrphanTarballs()

	if _, err := os.Stat(referenced); err != nil {
		t.Errorf("referenced tarball was removed: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("in-grace tarball was removed: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan tarball survived; stat err=%v (want not-exist)", err)
	}
}

func TestImageReadyTarball(t *testing.T) {
	dir := t.TempDir()
	mgr := &Manager{lxcPath: filepath.Join(dir, "lxc")}

	tb := filepath.Join(dir, "img.tar.gz")
	if err := os.WriteFile(tb, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Tarball present → ready, even though the legacy template dir
	// (lxcPath/<TemplateName>/config) does NOT exist. This is the regression
	// guard: before the fix, a tarball image fell through to the legacy probe
	// and was reported not-ready, triggering a duplicate re-pull.
	ready := &store.ImageRecord{TemplateTarball: tb, TemplateName: "__template_oci_busybox"}
	if !mgr.ImageReady(ready) {
		t.Error("tarball-backed image with an existing tarball must be ready")
	}

	// Tarball missing → not ready.
	missing := &store.ImageRecord{TemplateTarball: filepath.Join(dir, "gone.tar.gz"), TemplateName: "__template_oci_busybox"}
	if mgr.ImageReady(missing) {
		t.Error("tarball-backed image with a missing tarball must not be ready")
	}

	if mgr.ImageReady(nil) {
		t.Error("nil record must not be ready")
	}
}

func TestReapOrphanTarballsSkippedWhenNotPVE(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	mgr := &Manager{store: st} // pveStorage empty → not PVE mode

	dir := filepath.Join(st.RootDir(), "pve-templates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "orphan.tar.gz")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * orphanCTGrace)
	os.Chtimes(p, old, old)

	mgr.reapOrphanTarballs()

	if _, err := os.Stat(p); err != nil {
		t.Errorf("non-PVE mode must not touch tarballs: %v", err)
	}
}

func TestConfHasManagedTag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		conf string
		want bool
	}{
		{
			name: "sole managed tag",
			conf: "arch: amd64\nhostname: web\ntags: " + ManagedTag + "\n",
			want: true,
		},
		{
			name: "managed tag among others (semicolon-separated)",
			conf: "tags: prod;" + ManagedTag + ";gpu\n",
			want: true,
		},
		{
			name: "managed tag with surrounding whitespace",
			conf: "tags:   " + ManagedTag + "  \n",
			want: true,
		},
		{
			name: "no tags line at all",
			conf: "arch: amd64\nhostname: prod-db\n",
			want: false,
		},
		{
			name: "untagged production CT",
			conf: "tags: prod;postgres\n",
			want: false,
		},
		{
			name: "substring must not match (dld-managed-extra)",
			conf: "tags: " + ManagedTag + "-extra\n",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := confHasManagedTag([]byte(tc.conf)); got != tc.want {
				t.Fatalf("confHasManagedTag(%q) = %v, want %v", tc.conf, got, tc.want)
			}
		})
	}
}
