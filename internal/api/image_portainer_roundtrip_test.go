package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestImageConfigFromRecordForPortainer(t *testing.T) {
	t.Parallel()

	rec := &store.ImageRecord{
		OCIEntrypoint: []string{"/init"},
		OCICmd:        []string{"serve"},
		OCIEnv:        []string{"A=B"},
		OCIWorkingDir: "/work",
		OCIPorts:      []string{"8080/tcp"},
		OCILabels:     map[string]string{"app": "demo"},
		OCIUser:       "1000:1000",
		OCIStopSignal: "SIGTERM",
		OCIHealthcheck: &store.HealthcheckConfig{
			Test:        []string{"CMD-SHELL", "true"},
			Interval:    int64(time.Second),
			Timeout:     int64(2 * time.Second),
			StartPeriod: int64(3 * time.Second),
			Retries:     4,
		},
		OCIVolumes: []string{"/data"},
	}

	cfg := imageConfigFromRecord(rec)
	if cfg.User != "1000:1000" || cfg.StopSignal != "SIGTERM" || cfg.WorkingDir != "/work" {
		t.Fatalf("unexpected image config metadata: %+v", cfg)
	}
	if _, ok := cfg.ExposedPorts["8080/tcp"]; !ok {
		t.Fatalf("expected exposed port to round-trip, got %+v", cfg.ExposedPorts)
	}
	if _, ok := cfg.Volumes["/data"]; !ok {
		t.Fatalf("expected volume to round-trip, got %+v", cfg.Volumes)
	}
	if cfg.Healthcheck == nil || len(cfg.Healthcheck.Test) != 2 {
		t.Fatalf("expected healthcheck to round-trip, got %+v", cfg.Healthcheck)
	}
}

func TestSynthesiseImageConfigPreservesPortainerMetadata(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 4, 18, 10, 11, 12, 130000000, time.UTC)
	rec := &store.ImageRecord{
		Ref:           "docker.io/library/demo:latest",
		Release:       "jammy",
		Created:       created,
		OCIPorts:      []string{"8080/tcp"},
		OCIVolumes:    []string{"/data"},
		OCIUser:       "root",
		OCIStopSignal: "SIGTERM",
		OCILabels:     map[string]string{"role": "web"},
	}

	body, _, err := synthesiseImageConfig(rec, "abc123")
	if err != nil {
		t.Fatalf("synthesise image config: %v", err)
	}
	jsonText := string(body)
	for _, want := range []string{
		`"os.version":"jammy"`,
		`"container_config"`,
		`"ExposedPorts":{"8080/tcp":{}}`,
		`"Volumes":{"/data":{}}`,
		created.Format(time.RFC3339Nano),
	} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("expected %s in %s", want, jsonText)
		}
	}
}

func TestEffectiveSaveImageConfigFallsBackToContainerConfig(t *testing.T) {
	t.Parallel()

	cfg := saveImageConfig{
		ContainerConfig: saveImageConfigBody{
			User:       "root",
			Volumes:    map[string]struct{}{"/data": {}},
			StopSignal: "SIGTERM",
		},
	}
	effective := effectiveSaveImageConfig(cfg)
	if effective.User != "root" || effective.StopSignal != "SIGTERM" {
		t.Fatalf("expected container_config fallback, got %+v", effective)
	}
	if _, ok := effective.Volumes["/data"]; !ok {
		t.Fatalf("expected volumes to come from container_config, got %+v", effective.Volumes)
	}
}

func TestParseSavedImageCreatedPreservesTimestamp(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, 4, 18, 1, 2, 3, 456000000, time.UTC)
	got := parseSavedImageCreated(want.Format(time.RFC3339Nano))
	if !got.Equal(want) {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestExtractBundleTarAcceptsGzipBundles(t *testing.T) {
	t.Parallel()

	var raw bytes.Buffer
	gzw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gzw)
	payload := []byte(`[]`)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(payload))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	dest := t.TempDir()
	if err := extractBundleTar(bytes.NewReader(raw.Bytes()), dest); err != nil {
		t.Fatalf("extract gzip bundle: %v", err)
	}
	if _, err := os.Stat(dest + "/manifest.json"); err != nil {
		t.Fatalf("expected manifest.json after extract: %v", err)
	}
}

func TestLoadConfigJSONPreservesVolumesAndOSVersion(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"created":"2026-04-18T10:11:12.13Z",
		"os.version":"jammy",
		"container_config":{
			"Volumes":{"/data":{}},
			"User":"root",
			"StopSignal":"SIGTERM"
		}
	}`)
	var cfg saveImageConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal save image config: %v", err)
	}
	effective := effectiveSaveImageConfig(cfg)
	if cfg.OSVersion != "jammy" {
		t.Fatalf("expected os.version jammy, got %q", cfg.OSVersion)
	}
	if _, ok := effective.Volumes["/data"]; !ok {
		t.Fatalf("expected volume from config json, got %+v", effective.Volumes)
	}
	if effective.User != "root" || effective.StopSignal != "SIGTERM" {
		t.Fatalf("expected metadata fallback from container_config, got %+v", effective)
	}
}
