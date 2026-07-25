package oci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeBlob writes data under blobs/sha256/<name> and returns its "sha256:<name>"
// digest reference. The name need not be a real hash — parseImageConfig only
// uses it to locate the blob.
func writeBlob(t *testing.T, ociDir, name string, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal blob %s: %v", name, err)
	}
	dir := filepath.Join(ociDir, "blobs", "sha256")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("write blob %s: %v", name, err)
	}
	return "sha256:" + name
}

// writeLayout builds a minimal single-manifest OCI layout and returns its dir.
func writeLayout(t *testing.T, configDigestName string, cfg any) string {
	t.Helper()
	ociDir := t.TempDir()

	configDigest := writeBlob(t, ociDir, configDigestName, cfg)
	manifestDigest := writeBlob(t, ociDir, "manifestblob", map[string]any{
		"config": map[string]string{"digest": configDigest},
	})

	index := map[string]any{
		"manifests": []map[string]any{{"digest": manifestDigest}},
	}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ociDir, "index.json"), data, 0o644); err != nil {
		t.Fatalf("write index.json: %v", err)
	}
	return ociDir
}

// The config-blob digest is Docker's image ID. Dropping it on the floor is what
// left pulled images on the tag-derived "oci_ghcr_io_org_app_tag" pseudo-ID,
// which is not content addressed.
func TestParseImageConfigCapturesConfigDigest(t *testing.T) {
	const digestName = "aaaa111122223333444455556666777788889999aaaabbbbccccddddeeeeffff"
	ociDir := writeLayout(t, digestName, map[string]any{
		"config": map[string]any{"Cmd": []string{"/bin/sh"}},
	})

	cfg, err := parseImageConfig(ociDir, "latest")
	if err != nil {
		t.Fatalf("parseImageConfig: %v", err)
	}
	if want := "sha256:" + digestName; cfg.ConfigDigest != want {
		t.Errorf("ConfigDigest = %q, want %q", cfg.ConfigDigest, want)
	}
	// The rest of the config must still parse — the digest is additive.
	if len(cfg.Cmd) != 1 || cfg.Cmd[0] != "/bin/sh" {
		t.Errorf("Cmd = %v, want [/bin/sh]", cfg.Cmd)
	}
}

// Two images pushed to the same tag must yield different config digests, so
// `docker images` distinguishes them instead of collapsing both onto one ID.
func TestParseImageConfigDigestsDifferPerContent(t *testing.T) {
	oldDir := writeLayout(t, "1111111111111111111111111111111111111111111111111111111111111111",
		map[string]any{"config": map[string]any{"Cmd": []string{"/old"}}})
	newDir := writeLayout(t, "2222222222222222222222222222222222222222222222222222222222222222",
		map[string]any{"config": map[string]any{"Cmd": []string{"/new"}}})

	oldCfg, err := parseImageConfig(oldDir, "latest")
	if err != nil {
		t.Fatalf("parseImageConfig(old): %v", err)
	}
	newCfg, err := parseImageConfig(newDir, "latest")
	if err != nil {
		t.Fatalf("parseImageConfig(new): %v", err)
	}
	if oldCfg.ConfigDigest == newCfg.ConfigDigest {
		t.Fatalf("digests collided: both %q", oldCfg.ConfigDigest)
	}
}
