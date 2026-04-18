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
		},
		"",
		"",
		"",
		0,
	)
	if err != nil {
		t.Fatalf("buildLANConfig returned error: %v", err)
	}
	if cfg.Bridge != "vmbr0" {
		t.Fatalf("expected vmbr0, got %q", cfg.Bridge)
	}
	if cfg.Prefix != "10.10.0" {
		t.Fatalf("unexpected prefix %q", cfg.Prefix)
	}
}

func TestBuildLANConfigMultipleBridgeUnsupported(t *testing.T) {
	t.Parallel()

	_, err := buildLANConfig([]string{
		"vmbr0=10.10.0/24:10.10.0.1",
		"vmbr0=10.20.0/24:10.20.0.1",
	}, "", "", "", 0)
	if err == nil {
		t.Fatal("expected multi-bridge error")
	}
}

func TestBuildLANConfigLegacyBridge(t *testing.T) {
	t.Parallel()

	cfg, err := buildLANConfig(nil, "vmbr-legacy", "10.30.0", "10.30.0.1", 24)
	if err != nil {
		t.Fatalf("buildLANConfig returned error: %v", err)
	}
	if cfg.Bridge != "vmbr-legacy" {
		t.Fatalf("expected legacy bridge vmbr-legacy, got %q", cfg.Bridge)
	}
}
