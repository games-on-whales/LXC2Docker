package lxc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestImageReadyRequiresExistingTemplateSource(t *testing.T) {
	t.Parallel()

	lxcPath := t.TempDir()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	mgr := &Manager{lxcPath: lxcPath, pveStorage: "large", store: st}

	stale := &store.ImageRecord{
		Ref:          "portainer/portainer-ce:latest",
		TemplateName: "__template_oci_portainer_portainer-ce_latest",
	}
	if mgr.ImageReady(stale) {
		t.Fatal("expected stale image record without a template source to be reported unavailable")
	}

	configDir := filepath.Join(lxcPath, stale.TemplateName)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir template dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config"), []byte("lxc.rootfs.path = dir:/tmp/rootfs\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if !mgr.ImageReady(stale) {
		t.Fatal("expected existing legacy template config to be reported available")
	}
}

func TestSanitizeHostnameCapsAndRemovesInvalidChars(t *testing.T) {
	t.Parallel()

	got := sanitizeHostname("tmpl-ghcr.io_rakuensoftware_smoothnas-plugin-gh-runner_0.3.1")
	if len(got) > 63 {
		t.Fatalf("hostname length = %d, want <= 63: %q", len(got), got)
	}
	if got != "tmpl-ghcr-io-rakuensoftware-smoothnas-plugin-gh-runner-0-3-1" {
		t.Fatalf("hostname = %q", got)
	}
}
