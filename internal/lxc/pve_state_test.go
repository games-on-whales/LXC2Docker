package lxc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// TestStatePVEUsesLXCInfo pins the PVE state query to lxc-info against the PVE
// lxcpath (pct status costs ~0.8 s CPU per call and State is polled by /wait),
// and pins the mapping that keeps a suspended CT "running" so a pending wait
// does not return early.
func TestStatePVEUsesLXCInfo(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := st.AddContainer(&store.ContainerRecord{ID: id, Name: "wolf", VMID: 101}); err != nil {
		t.Fatalf("AddContainer: %v", err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "lxc-info-args")
	pctMarker := filepath.Join(t.TempDir(), "pct-called")
	lxcInfo := "#!/bin/sh\nprintf '%s ' \"$@\" > \"$ARGS_FILE\"\nprintf '%s\\n' \"$FAKE_STATE\"\nexit \"$FAKE_RC\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "lxc-info"), []byte(lxcInfo), 0o755); err != nil {
		t.Fatalf("write fake lxc-info: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "pct"), []byte("#!/bin/sh\ntouch \"$PCT_MARKER\"\necho 'status: running'\n"), 0o755); err != nil {
		t.Fatalf("write fake pct: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARGS_FILE", argsFile)
	t.Setenv("PCT_MARKER", pctMarker)

	// The manager's own lxcPath must not leak into PVE queries.
	mgr := &Manager{lxcPath: t.TempDir(), store: st}

	cases := []struct {
		name  string
		state string
		rc    string
		want  string
	}{
		{"running", "RUNNING", "0", "running"},
		{"stopped", "STOPPED", "0", "exited"},
		{"frozen", "FROZEN", "0", "running"},
		{"freezing", "FREEZING", "0", "running"},
		{"stopping", "STOPPING", "0", "running"},
		{"empty output", "", "0", "exited"},
		{"command failure", "", "1", "exited"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FAKE_STATE", tc.state)
			t.Setenv("FAKE_RC", tc.rc)
			os.Remove(argsFile)

			got, err := mgr.State(id)
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != tc.want {
				t.Errorf("State with lxc-info %q (rc %s) = %q, want %q", tc.state, tc.rc, got, tc.want)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("lxc-info was not called: %v", err)
			}
			if want := "-n 101 --lxcpath /var/lib/lxc -sH"; strings.TrimSpace(string(args)) != want {
				t.Errorf("lxc-info args = %q, want %q", strings.TrimSpace(string(args)), want)
			}
		})
	}
	if _, err := os.Stat(pctMarker); !os.IsNotExist(err) {
		t.Fatalf("State shelled out to pct: %v", err)
	}
}
