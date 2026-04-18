package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNormalizeHostConfigForPortainerInspect(t *testing.T) {
	t.Parallel()

	hc := &HostConfig{}
	normalizeHostConfig(hc)

	body, err := json.Marshal(hc)
	if err != nil {
		t.Fatalf("marshal host config: %v", err)
	}
	jsonText := string(body)

	for _, want := range []string{
		`"Mounts":[]`,
		`"Tmpfs":{}`,
		`"VolumesFrom":[]`,
		`"Sysctls":{}`,
		`"Dns":[]`,
		`"DnsOptions":[]`,
		`"DnsSearch":[]`,
		`"ExtraHosts":[]`,
		`"Ulimits":[]`,
		`"Annotations":{}`,
		`"LogConfig":{"Type":"json-file","Config":{}}`,
	} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("expected %s in %s", want, jsonText)
		}
	}
}

func TestNormalizeContainerConfigForPortainerInspect(t *testing.T) {
	t.Parallel()

	cfg := normalizeContainerConfig(&ContainerConfig{})
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal container config: %v", err)
	}
	jsonText := string(body)

	for _, want := range []string{
		`"ExposedPorts":{}`,
		`"Volumes":{}`,
		`"Cmd":[]`,
		`"Entrypoint":[]`,
		`"Env":[]`,
		`"Labels":{}`,
	} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("expected %s in %s", want, jsonText)
		}
	}
}

func TestArchiveHelpersMatchPortainerFileBrowserExpectations(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 4, 18, 12, 34, 56, 789000000, time.UTC)
	path := t.TempDir() + "/note.txt"
	if err := os.WriteFile(path, []byte("hello"), 0o640); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat temp file: %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(archivePathStatHeader(info))
	if err != nil {
		t.Fatalf("decode stat header: %v", err)
	}

	var stat struct {
		Name       string `json:"name"`
		Size       int64  `json:"size"`
		Mode       uint32 `json:"mode"`
		Mtime      string `json:"mtime"`
		LinkTarget string `json:"linkTarget"`
	}
	if err := json.Unmarshal(raw, &stat); err != nil {
		t.Fatalf("unmarshal stat header: %v", err)
	}
	if stat.Name != "note.txt" {
		t.Fatalf("expected note.txt name, got %q", stat.Name)
	}
	if stat.Size != 5 {
		t.Fatalf("expected size 5, got %d", stat.Size)
	}
	if stat.Mode != uint32(info.Mode()) {
		t.Fatalf("expected mode %d, got %d", uint32(info.Mode()), stat.Mode)
	}
	if stat.Mtime != ts.Format(time.RFC3339Nano) {
		t.Fatalf("expected mtime %q, got %q", ts.Format(time.RFC3339Nano), stat.Mtime)
	}
	if stat.LinkTarget != "" {
		t.Fatalf("expected empty link target, got %q", stat.LinkTarget)
	}
	if got := archiveBaseName("/var/log", fakeFileInfo{name: "log", dir: true}); got != "log" {
		t.Fatalf("expected directory basename log, got %q", got)
	}
}

func TestAdditionalSwarmHeadRoutesReturnUnavailable(t *testing.T) {
	t.Parallel()

	h := &Handler{
		attachPTYs: map[string]*os.File{},
		events:     newEventBroker(),
	}
	r := mustMuxRouter(t, h.routes())

	for _, path := range []string{
		"/v1.45/configs/config-1",
		"/v1.45/secrets/secret-1",
	} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, path, nil))
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503 for HEAD %s, got %d", path, rr.Code)
			}
		})
	}
}

type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string  { return f.name }
func (f fakeFileInfo) Size() int64   { return 0 }
func (f fakeFileInfo) Mode() os.FileMode {
	if f.dir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }
