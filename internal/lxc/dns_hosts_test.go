package lxc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestCanonicalNetwork(t *testing.T) {
	for _, in := range []string{"", "default", "bridge"} {
		if got := canonicalNetwork(in); got != DefaultNetworkName {
			t.Errorf("canonicalNetwork(%q) = %q, want %q", in, got, DefaultNetworkName)
		}
	}
	if got := canonicalNetwork("my-net"); got != "my-net" {
		t.Errorf("canonicalNetwork(my-net) = %q", got)
	}
}

func TestRenderManagedHostsBlock(t *testing.T) {
	got := renderManagedHostsBlock([]hostsPeer{
		{ip: "10.100.0.14", names: []string{"aimee-kb-postgres"}},
		{ip: "10.100.0.15", names: []string{"aimee-kb-kb", "kb"}},
	})
	// Sorted by first name (aimee-kb-kb < aimee-kb-postgres); aliases joined.
	want := "10.100.0.15\taimee-kb-kb kb\n10.100.0.14\taimee-kb-postgres\n"
	if got != want {
		t.Errorf("block =\n%q\nwant\n%q", got, want)
	}
	if renderManagedHostsBlock(nil) != "" {
		t.Error("empty peers should render empty block")
	}
}

func TestComposeHostsFilePreservesAndReplaces(t *testing.T) {
	image := "127.0.0.1\tlocalhost\n10.100.0.99 added-host\n"

	// First splice: managed block appended, image content preserved.
	v1 := composeHostsFile(image, "10.100.0.14\taimee-kb-postgres\n")
	if !strings.Contains(v1, "127.0.0.1\tlocalhost") || !strings.Contains(v1, "10.100.0.99 added-host") {
		t.Errorf("image content lost:\n%s", v1)
	}
	if !strings.Contains(v1, hostsManagedBegin) || !strings.Contains(v1, "aimee-kb-postgres") {
		t.Errorf("managed block missing:\n%s", v1)
	}

	// Second splice with a drifted IP: old managed block replaced, not duplicated.
	v2 := composeHostsFile(v1, "10.100.0.22\taimee-kb-postgres\n")
	if strings.Contains(v2, "10.100.0.14") {
		t.Errorf("stale managed entry survived:\n%s", v2)
	}
	if strings.Count(v2, hostsManagedBegin) != 1 {
		t.Errorf("managed block duplicated:\n%s", v2)
	}
	if !strings.Contains(v2, "127.0.0.1\tlocalhost") || !strings.Contains(v2, "added-host") {
		t.Errorf("image content lost after replace:\n%s", v2)
	}

	// Empty block removes the managed section entirely.
	v3 := composeHostsFile(v2, "")
	if strings.Contains(v3, hostsManagedBegin) {
		t.Errorf("managed block not removed:\n%s", v3)
	}
	if !strings.Contains(v3, "added-host") {
		t.Errorf("image content lost on removal:\n%s", v3)
	}
}

func newRec(id, name, ip string, networks ...string) *store.ContainerRecord {
	rec := &store.ContainerRecord{ID: id, Name: name, IPAddress: ip}
	if len(networks) > 0 {
		rec.Networks = map[string]store.NetworkAttachment{}
		for _, n := range networks {
			rec.Networks[n] = store.NetworkAttachment{NetworkID: n, IPAddress: ip}
		}
	}
	return rec
}

func TestHostsPeersForSharedNetworkOnly(t *testing.T) {
	m := &Manager{}
	kb := newRec("id-kb", "aimee-kb-kb", "10.0.0.2", "veth0")
	pg := newRec("id-pg", "aimee-kb-postgres", "10.0.0.3", "veth0")
	other := newRec("id-x", "isolated", "10.9.9.9", "private-net")
	noIP := newRec("id-z", "pending", "", "veth0")
	all := []*store.ContainerRecord{kb, pg, other, noIP}

	peers := m.hostsPeersFor(kb, all)
	names := map[string]string{}
	for _, p := range peers {
		names[p.names[0]] = p.ip
	}
	if names["aimee-kb-kb"] != "10.0.0.2" {
		t.Errorf("self should be present: %v", names)
	}
	if names["aimee-kb-postgres"] != "10.0.0.3" {
		t.Errorf("same-network peer missing: %v", names)
	}
	if _, ok := names["isolated"]; ok {
		t.Errorf("peer on a different network leaked: %v", names)
	}
	if _, ok := names["pending"]; ok {
		t.Errorf("peer with no IP should be skipped: %v", names)
	}
}

func TestSyncHostsWritesRootfsFiles(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{lxcPath: t.TempDir(), store: st}

	mk := func(id, name, ip string) {
		rec := newRec(id, name, ip, "veth0")
		if err := st.AddContainer(rec); err != nil {
			t.Fatal(err)
		}
		etc := filepath.Join(m.lxcPath, id, "rootfs", "etc")
		if err := os.MkdirAll(etc, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(etc, "hosts"), []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("id-kb", "aimee-kb-kb", "10.100.0.15")
	mk("id-pg", "aimee-kb-postgres", "10.100.0.14")

	m.syncHosts()

	kbHosts, err := os.ReadFile(filepath.Join(m.lxcPath, "id-kb", "rootfs", "etc", "hosts"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(kbHosts)
	if !strings.Contains(body, "127.0.0.1 localhost") {
		t.Errorf("localhost line dropped:\n%s", body)
	}
	for _, want := range []string{"10.100.0.15\taimee-kb-kb", "10.100.0.14\taimee-kb-postgres"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}

	// Idempotent: a second sweep with no IP change must not duplicate.
	m.syncHosts()
	again, _ := os.ReadFile(filepath.Join(m.lxcPath, "id-kb", "rootfs", "etc", "hosts"))
	if strings.Count(string(again), hostsManagedBegin) != 1 {
		t.Errorf("managed block duplicated after re-sync:\n%s", again)
	}

	// Drift: postgres moves; the kb's /etc/hosts must follow.
	pg := st.GetContainer("id-pg")
	pg.IPAddress = "10.100.0.30"
	pg.Networks["veth0"] = store.NetworkAttachment{NetworkID: "veth0", IPAddress: "10.100.0.30"}
	if err := st.AddContainer(pg); err != nil {
		t.Fatal(err)
	}
	m.syncHosts()
	drifted, _ := os.ReadFile(filepath.Join(m.lxcPath, "id-kb", "rootfs", "etc", "hosts"))
	if !strings.Contains(string(drifted), "10.100.0.30\taimee-kb-postgres") {
		t.Errorf("drifted peer IP not reflected:\n%s", drifted)
	}
	if strings.Contains(string(drifted), "10.100.0.14") {
		t.Errorf("stale peer IP survived:\n%s", drifted)
	}
}
