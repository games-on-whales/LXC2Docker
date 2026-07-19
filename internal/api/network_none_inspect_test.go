package api

import (
	"encoding/json"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// A --network none container is an isolated netns (loopback only). Its inspect
// must not advertise a managed-bridge address or endpoint — doing so falsely
// signals egress and makes isolation probes reject a genuinely isolated sandbox.
func TestInspectNetworkSettingsNoneModeIsNetworkless(t *testing.T) {
	t.Parallel()

	raw, _ := json.Marshal(HostConfig{NetworkMode: "none"})
	rec := &store.ContainerRecord{IPAddress: "10.100.0.32", RawHostConfig: raw}

	if !isNoneNetworkMode(rec) {
		t.Fatal("isNoneNetworkMode = false, want true for NetworkMode=none")
	}
	ns := inspectNetworkSettings(rec, nil)
	if ns.IPAddress != "" {
		t.Errorf("IPAddress = %q, want empty for --network none", ns.IPAddress)
	}
	if ns.Gateway != "" {
		t.Errorf("Gateway = %q, want empty for --network none", ns.Gateway)
	}
	for name, ep := range ns.Networks {
		if name != "none" {
			t.Errorf("unexpected network %q for --network none", name)
		}
		if ep.IPAddress != "" {
			t.Errorf("network %q has IPAddress %q, want empty", name, ep.IPAddress)
		}
	}
}

// A normal bridged container must still report its managed-bridge address so
// existing clients keep working — the none-mode gate must not regress it.
func TestInspectNetworkSettingsBridgedKeepsAddress(t *testing.T) {
	t.Parallel()

	rec := &store.ContainerRecord{
		IPAddress: "10.100.0.10",
		Networks: map[string]store.NetworkAttachment{
			"veth0": {NetworkID: "veth0", IPAddress: "10.100.0.10", Gateway: "10.100.0.1"},
		},
	}
	if isNoneNetworkMode(rec) {
		t.Fatal("isNoneNetworkMode = true, want false for a bridged container")
	}
	ns := inspectNetworkSettings(rec, nil)
	if ns.IPAddress != "10.100.0.10" {
		t.Errorf("IPAddress = %q, want 10.100.0.10", ns.IPAddress)
	}
	if _, ok := ns.Networks["veth0"]; !ok {
		t.Errorf("expected veth0 endpoint to survive, got %#v", ns.Networks)
	}
}
