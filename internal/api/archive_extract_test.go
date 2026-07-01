package api

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// buildTar builds an in-memory tar from the given entries.
func buildTar(t *testing.T, entries []*tar.Header, bodies map[string]string) *tar.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if b, ok := bodies[h.Name]; ok {
			if _, err := tw.Write([]byte(b)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	return tar.NewReader(&buf)
}

func TestExtractArchiveBasic(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	tr := buildTar(t, []*tar.Header{
		{Name: "sub/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "sub/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
	}, map[string]string{"sub/file.txt": "hello"})

	if e := extractArchive(dest, tr, false, false); e != nil {
		t.Fatalf("extractArchive: %d %s", e.code, e.msg)
	}
	got, err := os.ReadFile(filepath.Join(dest, "sub", "file.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("file = %q, err %v; want %q", got, err, "hello")
	}
}

func TestExtractArchiveNoOverwriteDirNonDir(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	// Pre-create a directory that the archive will try to replace with a file.
	if err := os.Mkdir(filepath.Join(dest, "clash"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := buildTar(t, []*tar.Header{
		{Name: "clash", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
	}, map[string]string{"clash": "no"})

	e := extractArchive(dest, tr, true, false)
	if e == nil || e.code != 400 {
		t.Fatalf("expected 400 conflict, got %+v", e)
	}
	// The existing directory must be untouched (not replaced).
	info, err := os.Lstat(filepath.Join(dest, "clash"))
	if err != nil || !info.IsDir() {
		t.Fatalf("clash should still be a directory, err %v", err)
	}
}

func TestExtractArchiveOverwriteAllowedWithoutFlag(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	if err := os.Mkdir(filepath.Join(dest, "clash"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := buildTar(t, []*tar.Header{
		// A directory entry over an existing directory is fine even with the flag.
		{Name: "clash", Typeflag: tar.TypeDir, Mode: 0o755},
	}, nil)
	if e := extractArchive(dest, tr, true, false); e != nil {
		t.Fatalf("dir-over-dir should be allowed, got %+v", e)
	}
}

func TestExtractArchiveSkipsSymlinks(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	tr := buildTar(t, []*tar.Header{
		{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	}, nil)
	if e := extractArchive(dest, tr, false, false); e != nil {
		t.Fatalf("symlink entry should be skipped, got %+v", e)
	}
	if _, err := os.Lstat(filepath.Join(dest, "evil")); !os.IsNotExist(err) {
		t.Fatalf("symlink should not have been created")
	}
}

func TestFileKind(t *testing.T) {
	t.Parallel()
	if fileKind(true) != "directory" || fileKind(false) != "file" {
		t.Fatal("fileKind mapping wrong")
	}
}
