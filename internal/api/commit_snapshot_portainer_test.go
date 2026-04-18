package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotCommittedImageRootfsCreatesTemplateForPortainer(t *testing.T) {
	t.Parallel()

	lxcPath := t.TempDir()
	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir rootfs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc", "issue"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write rootfs file: %v", err)
	}

	templateName, err := snapshotCommittedImageRootfs(lxcPath, "example/app:latest", rootfs)
	if err != nil {
		t.Fatalf("snapshot committed image rootfs: %v", err)
	}
	if !strings.HasPrefix(templateName, "__template_commit_") {
		t.Fatalf("expected commit template prefix, got %q", templateName)
	}

	targetDir := filepath.Join(lxcPath, templateName)
	data, err := os.ReadFile(filepath.Join(targetDir, "rootfs", "etc", "issue"))
	if err != nil {
		t.Fatalf("read copied rootfs file: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected copied rootfs contents, got %q", string(data))
	}

	cfg, err := os.ReadFile(filepath.Join(targetDir, "config"))
	if err != nil {
		t.Fatalf("read template config: %v", err)
	}
	if !strings.Contains(string(cfg), "lxc.rootfs.path = dir:"+filepath.Join(targetDir, "rootfs")) {
		t.Fatalf("expected config to point at copied rootfs, got %q", string(cfg))
	}
}
