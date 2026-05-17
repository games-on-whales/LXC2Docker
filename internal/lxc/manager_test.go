package lxc

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCloneLegacyTemplateByCopyWritesContainerRootfsConfig(t *testing.T) {
	t.Parallel()

	lxcPath := t.TempDir()
	mgr := &Manager{lxcPath: lxcPath}
	templateName := "__template_oci_example_app_latest"
	templateRootfs := filepath.Join(lxcPath, templateName, "rootfs")
	if err := os.MkdirAll(filepath.Join(templateRootfs, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir template rootfs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(templateRootfs, "etc", "issue"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write template file: %v", err)
	}

	id := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if err := mgr.cloneLegacyTemplateByCopy(templateName, id); err != nil {
		t.Fatalf("clone by copy: %v", err)
	}

	containerRootfs := filepath.Join(lxcPath, id, "rootfs")
	got, err := os.ReadFile(filepath.Join(containerRootfs, "etc", "issue"))
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("copied file = %q", string(got))
	}

	cfg, err := os.ReadFile(filepath.Join(lxcPath, id, "config"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	body := string(cfg)
	if !strings.Contains(body, "lxc.rootfs.path = dir:"+containerRootfs) {
		t.Fatalf("config did not point at container rootfs: %s", body)
	}
	if strings.Contains(body, templateRootfs) {
		t.Fatalf("config still references template rootfs: %s", body)
	}
	if !strings.Contains(body, "lxc.uts.name = "+sanitizeHostname(id)) {
		t.Fatalf("config missing sanitized hostname: %s", body)
	}
}

func TestSmoothNASPluginLabelsAreGCProtected(t *testing.T) {
	t.Parallel()

	managed := &store.ContainerRecord{Labels: map[string]string{"io.smoothnas.managed": "true"}}
	if !smoothNASManagedContainer(managed) {
		t.Fatal("expected SmoothNAS managed plugin container to be GC-protected")
	}

	worker := &store.ContainerRecord{Labels: map[string]string{"io.smoothnas.gh-runner.worker": "true"}}
	if !smoothNASRunnerWorker(worker) {
		t.Fatal("expected SmoothNAS runner worker to be excluded from orphan support cleanup")
	}

	plain := &store.ContainerRecord{Labels: map[string]string{"io.smoothnas.plugin": "gh-runner"}}
	if smoothNASManagedContainer(plain) || smoothNASRunnerWorker(plain) {
		t.Fatal("non-owner SmoothNAS labels should not opt into GC protection")
	}
}
