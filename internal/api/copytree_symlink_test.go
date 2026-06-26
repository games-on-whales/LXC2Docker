package api

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression test for the `docker commit` failure
//
//	snapshot container rootfs: write .../rootfs/bin: copy_file_range: is a directory
//
// copyTree must recreate usrmerge-style symlinks (e.g. /bin -> usr/bin) instead
// of following them into a directory and running copyFile/io.Copy on it.
func TestCopyTreeRecreatesUsrmergeSymlinks(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")

	// usr/bin/sh is a real file; bin is a symlink to the usr/bin directory.
	if err := os.MkdirAll(filepath.Join(src, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "usr", "bin", "sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/bin", filepath.Join(src, "bin")); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree returned error (the copy_file_range-on-directory bug): %v", err)
	}

	// dst/bin must remain a symlink, not a copied directory.
	fi, err := os.Lstat(filepath.Join(dst, "bin"))
	if err != nil {
		t.Fatalf("lstat dst/bin: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dst/bin should be a symlink, got mode %v", fi.Mode())
	}
	if link, err := os.Readlink(filepath.Join(dst, "bin")); err != nil || link != "usr/bin" {
		t.Fatalf("dst/bin symlink target = %q (err=%v), want %q", link, err, "usr/bin")
	}

	// The real file under usr/bin must be copied through.
	if b, err := os.ReadFile(filepath.Join(dst, "usr", "bin", "sh")); err != nil || string(b) != "#!/bin/sh\n" {
		t.Fatalf("usr/bin/sh not copied correctly: %q err=%v", string(b), err)
	}
}
