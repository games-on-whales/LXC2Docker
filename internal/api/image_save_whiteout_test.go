package api

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"
)

// writeLayer writes a minimal layer tar with the given name->content entries.
func writeLayer(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	// Own entries as the current user so ApplyLayer's lchown succeeds when the
	// test runs unprivileged (production runs as root, where image uid/gid 0 is
	// preserved). Whiteout handling under test is independent of ownership.
	uid, gid := os.Getuid(), os.Getgid()
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
			Uid: uid, Gid: gid,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestExtractTarInto_Whiteout verifies the layer applier honours the OCI/aufs
// whiteout convention: a `.wh.<name>` entry in an upper layer deletes <name>
// from the lower layer and is itself NOT materialised. Plain `tar -xf` (the old
// implementation) failed both: it kept a.txt and left a literal `.wh.a.txt`.
func TestExtractTarInto_Whiteout(t *testing.T) {
	stage := t.TempDir()
	dst := t.TempDir()
	base := filepath.Join(stage, "base.tar")
	upper := filepath.Join(stage, "upper.tar")
	writeLayer(t, base, map[string]string{"a.txt": "A", "keep.txt": "K"})
	writeLayer(t, upper, map[string]string{".wh.a.txt": ""})

	if err := extractTarInto(base, dst); err != nil {
		t.Fatalf("apply base layer: %v", err)
	}
	if err := extractTarInto(upper, dst); err != nil {
		t.Fatalf("apply upper layer: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("a.txt should be whited-out (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "keep.txt")); err != nil {
		t.Errorf("keep.txt should survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".wh.a.txt")); !os.IsNotExist(err) {
		t.Errorf(".wh.a.txt whiteout marker must not be materialised (stat err=%v)", err)
	}
}
