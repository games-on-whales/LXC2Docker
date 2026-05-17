package oci

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestParseImageConfigReadsUserAndVolumes(t *testing.T) {
	ociDir := t.TempDir()
	writeBlob(t, ociDir, "sha256:manifest", `{
		"config": {"digest": "sha256:config"}
	}`)
	writeBlob(t, ociDir, "sha256:config", `{
		"config": {
			"Entrypoint": ["/entry"],
			"Cmd": ["run"],
			"Env": ["A=B"],
			"User": "1000:1000",
			"WorkingDir": "/work",
			"Volumes": {"/data": {}, "/work/cache": {}},
			"ExposedPorts": {"8080/tcp": {}}
		}
	}`)
	if err := os.WriteFile(filepath.Join(ociDir, "index.json"), []byte(`{
		"manifests": [{
			"digest": "sha256:manifest",
			"annotations": {"org.opencontainers.image.ref.name": "latest"}
		}]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := parseImageConfig(ociDir, "latest")
	if err != nil {
		t.Fatalf("parseImageConfig: %v", err)
	}
	if cfg.User != "1000:1000" {
		t.Fatalf("User = %q", cfg.User)
	}
	gotVolumes := append([]string{}, cfg.Volumes...)
	sort.Strings(gotVolumes)
	wantVolumes := []string{"/data", "/work/cache"}
	if !reflect.DeepEqual(gotVolumes, wantVolumes) {
		t.Fatalf("Volumes = %v want %v", gotVolumes, wantVolumes)
	}
}

func writeBlob(t *testing.T, ociDir, digest, body string) {
	t.Helper()
	path := filepath.Join(ociDir, "blobs", digestToPath(digest))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
