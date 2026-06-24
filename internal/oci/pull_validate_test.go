package oci

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateRootfs covers the post-unpack completeness guard: a rootfs must
// expose a resolvable POSIX shell, else the extraction is treated as incomplete
// (a dropped base layer) and rejected rather than cached as a ready template.
func TestValidateRootfs(t *testing.T) {
	t.Run("usr-merged complete", func(t *testing.T) {
		root := t.TempDir()
		mustMkdir(t, filepath.Join(root, "usr/bin"))
		mustWrite(t, filepath.Join(root, "usr/bin/dash"))
		mustSymlink(t, "dash", filepath.Join(root, "usr/bin/sh"))
		mustSymlink(t, "usr/bin", filepath.Join(root, "bin")) // /bin -> usr/bin
		if err := validateRootfs(root); err != nil {
			t.Fatalf("usr-merged rootfs should validate, got %v", err)
		}
	})

	t.Run("plain /bin/bash", func(t *testing.T) {
		root := t.TempDir()
		mustMkdir(t, filepath.Join(root, "bin"))
		mustWrite(t, filepath.Join(root, "bin/bash"))
		if err := validateRootfs(root); err != nil {
			t.Fatalf("rootfs with /bin/bash should validate, got %v", err)
		}
	})

	t.Run("base layer dropped (no shell)", func(t *testing.T) {
		// Mirrors the observed failure: only upper layers present (code-server +
		// app), no base OS — must be rejected.
		root := t.TempDir()
		mustMkdir(t, filepath.Join(root, "usr/bin"))
		mustWrite(t, filepath.Join(root, "usr/bin/code-server"))
		mustMkdir(t, filepath.Join(root, "usr/local/bin"))
		mustWrite(t, filepath.Join(root, "usr/local/bin/app"))
		if err := validateRootfs(root); err == nil {
			t.Fatal("rootfs without a shell must be rejected")
		}
	})

	t.Run("dangling /bin/sh (missing interpreter)", func(t *testing.T) {
		// A shell symlink whose target was dropped resolves to ENOENT on exec;
		// os.Stat follows the link, so this is correctly rejected.
		root := t.TempDir()
		mustMkdir(t, filepath.Join(root, "bin"))
		mustSymlink(t, "dash", filepath.Join(root, "bin/sh")) // dash absent
		if err := validateRootfs(root); err == nil {
			t.Fatal("dangling /bin/sh must be rejected")
		}
	})
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
