package lxc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestEnsureRuntimeDir_ParentsTraversableLeaf0700(t *testing.T) {
	rootfs := t.TempDir()
	// Simulate a restrictive intermediate dir baked into an image/template;
	// os.MkdirAll would leave it untouched, so the chmod pass must repair it.
	if err := os.MkdirAll(filepath.Join(rootfs, "var", "lib", "smoothnas"), 0o700); err != nil {
		t.Fatal(err)
	}

	ensureRuntimeDir(rootfs, "/var/lib/smoothnas/plugins/wolf/runtime")

	// Every parent component must be traversable by "other" (o+x) so a
	// non-root run-user can reach into the runtime dir.
	for _, rel := range []string{
		"var", "var/lib", "var/lib/smoothnas",
		"var/lib/smoothnas/plugins", "var/lib/smoothnas/plugins/wolf",
	} {
		fi, err := os.Stat(filepath.Join(rootfs, rel))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if fi.Mode().Perm()&0o001 == 0 {
			t.Errorf("%s mode %#o is not world-traversable", rel, fi.Mode().Perm())
		}
	}

	// The leaf itself keeps the spec-mandated 0700.
	leaf, err := os.Stat(filepath.Join(rootfs, "var/lib/smoothnas/plugins/wolf/runtime"))
	if err != nil {
		t.Fatalf("stat leaf: %v", err)
	}
	if leaf.Mode().Perm() != 0o700 {
		t.Errorf("leaf mode = %#o, want 0700", leaf.Mode().Perm())
	}
}

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

func TestLXCInfoLinkParsesHostVeth(t *testing.T) {
	t.Parallel()

	out := `Name:           abc123
State:          RUNNING
PID:            1234
IP:             10.100.0.2
Link:           vethABCDEF
`
	if got := lxcInfoLink(out); got != "vethABCDEF" {
		t.Fatalf("lxcInfoLink = %q", got)
	}
	if got := lxcInfoLink("State: RUNNING\n"); got != "" {
		t.Fatalf("expected empty link, got %q", got)
	}
}

func TestBridgedNICConfigured(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  string
		want bool
	}{
		{"veth bridged", "lxc.net.0.type = veth\nlxc.net.0.link = br0\n", true},
		{"network none (empty)", "lxc.rootfs.path = dir:/x\nlxc.net.0.type = empty\n", false},
		{"network host (none)", "lxc.net.0.type = none\n", false},
		{"no network configured", "lxc.rootfs.path = dir:/x\nlxc.uts.name = c\n", false},
		{"veth without spaces", "lxc.net.0.type=veth\n", true},
	}
	for _, tc := range cases {
		if got := bridgedNICConfigured(tc.cfg); got != tc.want {
			t.Errorf("%s: bridgedNICConfigured = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildResolvConfUsesHostNameservers(t *testing.T) {
	dir := t.TempDir()
	hostResolv := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(hostResolv, []byte("nameserver 192.168.1.1\nsearch lan\n"), 0o644); err != nil {
		t.Fatalf("write host resolv: %v", err)
	}
	old := hostResolvConfPaths
	hostResolvConfPaths = []string{hostResolv}
	t.Cleanup(func() { hostResolvConfPaths = old })

	got := buildResolvConf(ContainerConfig{})
	if got != "nameserver 192.168.1.1\noptions timeout:2 attempts:2 rotate\n" {
		t.Fatalf("resolv.conf = %q", got)
	}
}

func TestBuildResolvConfSkipsHostLoopbackStub(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.conf")
	uplink := filepath.Join(dir, "uplink.conf")
	if err := os.WriteFile(stub, []byte("nameserver 127.0.0.53\n"), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	if err := os.WriteFile(uplink, []byte("nameserver 192.168.1.1\nnameserver 1.1.1.1\n"), 0o644); err != nil {
		t.Fatalf("write uplink: %v", err)
	}
	old := hostResolvConfPaths
	hostResolvConfPaths = []string{stub, uplink}
	t.Cleanup(func() { hostResolvConfPaths = old })

	got := buildResolvConf(ContainerConfig{})
	if got != "nameserver 192.168.1.1\nnameserver 1.1.1.1\noptions timeout:2 attempts:2 rotate\n" {
		t.Fatalf("resolv.conf = %q", got)
	}
}

func TestBuildResolvConfPreservesExplicitDockerDNS(t *testing.T) {
	got := buildResolvConf(ContainerConfig{DNS: []string{"9.9.9.9"}})
	if got != "nameserver 9.9.9.9\n" {
		t.Fatalf("resolv.conf = %q", got)
	}
}

func TestRemoveContainerMissingLXCDropsStoreEntry(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	id := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if err := st.AddContainer(&store.ContainerRecord{ID: id, Name: "gone"}); err != nil {
		t.Fatalf("add container: %v", err)
	}
	mgr := &Manager{lxcPath: t.TempDir(), store: st}

	if err := mgr.RemoveContainer(id); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if got := st.GetContainer(id); got != nil {
		t.Fatalf("container record still present: %+v", got)
	}
}

func TestLXCDestroyMissingOutput(t *testing.T) {
	t.Parallel()

	out := []byte("lxc-destroy: abc: ../src/lxc/tools/lxc_destroy.c: lxc_destroy_main: 241 Container is not defined\n")
	if !lxcDestroyMissing(out) {
		t.Fatal("expected missing LXC destroy output to be recognized")
	}
	if lxcDestroyMissing([]byte("permission denied")) {
		t.Fatal("unrelated destroy errors should not be treated as missing containers")
	}
}

func TestParseChownEntry(t *testing.T) {
	cases := []struct {
		in       string
		path     string
		uid, gid int
	}{
		{"/run/wolf", "/run/wolf", 0, 0},            // default owner is root
		{"/run/wolf:1000", "/run/wolf", 1000, 1000}, // uid only => gid=uid
		{"/run/wolf:1000:2000", "/run/wolf", 1000, 2000},
		{" /run/wolf : 0 : 0 ", "/run/wolf", 0, 0}, // trims whitespace
		{"/run/wolf:bad", "/run/wolf", 0, 0},       // non-numeric uid ignored
	}
	for _, c := range cases {
		path, uid, gid := parseChownEntry(c.in)
		if path != c.path || uid != c.uid || gid != c.gid {
			t.Errorf("parseChownEntry(%q) = (%q,%d,%d); want (%q,%d,%d)",
				c.in, path, uid, gid, c.path, c.uid, c.gid)
		}
	}
}

func TestApplyChownLabels_NoLabelNoop(t *testing.T) {
	// A container without the dld.chown label must not error or touch anything.
	dir := t.TempDir()
	st, err := store.NewAt(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	st.AddContainer(&store.ContainerRecord{ID: "abc123", Name: "c", Labels: map[string]string{}})
	m := &Manager{store: st}
	m.applyChownLabels("abc123") // must be a no-op, no panic
}
