package main

import (
	"testing"
)

func TestParseBridgeSpec(t *testing.T) {
	t.Parallel()

	got, err := parseBridgeSpec("vmbr0=192.168.1/24:192.168.1.1")
	if err != nil {
		t.Fatalf("parseBridgeSpec returned error: %v", err)
	}
	if got.Name != "vmbr0" {
		t.Fatalf("expected vmbr0, got %q", got.Name)
	}
	if got.Prefix != "192.168.1" || got.Subnet != 24 {
		t.Fatalf("unexpected bridge subnet/prefix: %#v", got)
	}
	if got.Gateway != "192.168.1.1" {
		t.Fatalf("unexpected gateway %q", got.Gateway)
	}
}

func TestParseBridgeSpecRejectsInvalid(t *testing.T) {
	t.Parallel()

	cases := []string{
		"novar",
		"vmbr0=192.168.1/24",
		"vmbr0=192.168.1:192.168.1.1",
	}
	for _, raw := range cases {
		if _, err := parseBridgeSpec(raw); err == nil {
			t.Fatalf("expected parseBridgeSpec to fail for %q", raw)
		}
	}
}

func TestBuildLANConfig(t *testing.T) {
	t.Parallel()

	cfg, err := buildLANConfig(
		[]string{
			"vmbr0=10.10.0/24:10.10.0.1",
			"vmbr1=10.20.0/24:10.20.0.1",
		},
		"",
		"",
		"",
		0,
	)
	if err != nil {
		t.Fatalf("buildLANConfig returned error: %v", err)
	}
	if len(cfg.Bridges) != 2 {
		t.Fatalf("expected 2 bridges, got %d", len(cfg.Bridges))
	}
	if cfg.Default != "vmbr0" {
		t.Fatalf("expected first bridge as default, got %q", cfg.Default)
	}
}

func TestBuildLANConfigDuplicateBridge(t *testing.T) {
	t.Parallel()

	_, err := buildLANConfig([]string{
		"vmbr0=10.10.0/24:10.10.0.1",
		"vmbr0=10.20.0/24:10.20.0.1",
	}, "", "", "", 0)
	if err == nil {
		t.Fatal("expected duplicate bridge error")
	}
}

func TestBuildLANConfigLegacyBridge(t *testing.T) {
	t.Parallel()

	cfg, err := buildLANConfig(nil, "vmbr-legacy", "10.30.0", "10.30.0.1", 24)
	if err != nil {
		t.Fatalf("buildLANConfig returned error: %v", err)
	}
	if _, ok := cfg.Bridges["vmbr-legacy"]; !ok {
		t.Fatal("legacy bridge not present")
	}
}
