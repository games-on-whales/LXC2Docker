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
// and pins the Docker state mapping: frozen is "paused", and the transitional
// states never read as "exited" while the init is alive.
func TestStatePVEUsesLXCInfo(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := st.AddContainer(&store.ContainerRecord{ID: id, Name: "wolf", VMID: 101}); err != nil {
		t.Fatalf("AddContainer: %v", err)
	}
	// CT 101 is this container's, whatever the host's own /etc/pve holds.
	writeCT(t, fakePVE(t), 101, id)

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
		{"frozen", "FROZEN", "0", "paused"},
		{"freezing", "FREEZING", "0", "paused"},
		{"thawed", "THAWED", "0", "running"},
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

// Legacy containers share the mapping; they used to return lxc-info's raw
// state, so a STOPPING container read as not running and ended a /wait before
// it had stopped.
func TestStateLegacyUsesTheSameMapping(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	id := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	if err := st.AddContainer(&store.ContainerRecord{ID: id, Name: "legacy"}); err != nil {
		t.Fatalf("AddContainer: %v", err)
	}
	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "lxc-info-args")
	lxcInfo := "#!/bin/sh\nprintf '%s ' \"$@\" > \"$ARGS_FILE\"\nprintf '%s\\n' \"$FAKE_STATE\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "lxc-info"), []byte(lxcInfo), 0o755); err != nil {
		t.Fatalf("write fake lxc-info: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARGS_FILE", argsFile)
	lxcPath := t.TempDir()
	mgr := &Manager{lxcPath: lxcPath, store: st}

	for state, want := range map[string]string{"STOPPING": "running", "FROZEN": "paused", "STOPPED": "exited"} {
		t.Setenv("FAKE_STATE", state)
		if got, _ := mgr.State(id); got != want {
			t.Errorf("State with lxc-info %q = %q, want %q", state, got, want)
		}
	}
	args, _ := os.ReadFile(argsFile)
	if want := "-n " + id + " --lxcpath " + lxcPath + " -sH"; strings.TrimSpace(string(args)) != want {
		t.Errorf("lxc-info args = %q, want %q", strings.TrimSpace(string(args)), want)
	}
}
