package lxc

import (
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestParseExitLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		line string
		name string
		code int
		ok   bool
	}{
		{"'101' exited with status [3]", "101", 3, true},
		{"'101' exited with status [137]", "101", 137, true},
		{"'0123abcd' exited with status [0]", "0123abcd", 0, true},
		{"'101' changed state to [STOPPED]", "", 0, false},
		{"garbage", "", 0, false},
	}
	for _, tc := range cases {
		name, code, ok := parseExitLine(tc.line)
		if name != tc.name || code != tc.code || ok != tc.ok {
			t.Errorf("parseExitLine(%q) = %q, %d, %v; want %q, %d, %v",
				tc.line, name, code, ok, tc.name, tc.code, tc.ok)
		}
	}
}

func exitMonitorFixture(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	for _, rec := range []*store.ContainerRecord{
		{ID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Name: "pve", VMID: 101},
		{ID: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210", Name: "legacy"},
		{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "stopped", VMID: 102, StoppedByUser: true, ExitCode: 143},
	} {
		if err := st.AddContainer(rec); err != nil {
			t.Fatalf("AddContainer: %v", err)
		}
	}
	return &Manager{lxcPath: t.TempDir(), store: st}, st
}

func TestRecordExitStoresTheCodeOfANaturalExit(t *testing.T) {
	m, st := exitMonitorFixture(t)

	m.recordExit("101", 3) // a PVE CT is named by its VMID
	if rec := st.GetContainer("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); rec.ExitCode != 3 || rec.FinishedAt == nil {
		t.Fatalf("PVE CT after exit 3: ExitCode=%d FinishedAt=%v", rec.ExitCode, rec.FinishedAt)
	}

	m.recordExit("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210", 42) // legacy: by ID
	if rec := st.GetContainer("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"); rec.ExitCode != 42 {
		t.Fatalf("legacy container after exit 42: ExitCode=%d", rec.ExitCode)
	}
}

// A stop or kill records the signal's code before the CT goes down; the init's
// own status (often 0 after a graceful SIGTERM, or 0 for a SIGKILL as
// lxc-monitor reports it) must not overwrite it.
func TestRecordExitKeepsTheCodeOfAUserStop(t *testing.T) {
	m, st := exitMonitorFixture(t)

	m.recordExit("102", 0)
	if rec := st.GetContainer("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); rec.ExitCode != 143 {
		t.Fatalf("user-stopped CT ExitCode = %d, want the recorded 143", rec.ExitCode)
	}
}

func TestRecordExitIgnoresUnknownContainers(t *testing.T) {
	m, _ := exitMonitorFixture(t)
	m.recordExit("999", 1)      // a CT the daemon does not manage
	m.recordExit("not-ours", 1) // a foreign legacy container
}
