package lxc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

const (
	vanishedID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	otherID    = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// fakePVE points pveDir at a mounted-looking /etc/pve lookalike (with the
// .version pmxcfs serves) and returns it.
func fakePVE(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lxc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".version"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := pveDir
	pveDir = dir
	t.Cleanup(func() { pveDir = saved })
	return dir
}

// writeCT writes a CT config the way the daemon does, naming its container id.
func writeCT(t *testing.T, dir string, vmid int, id string) {
	t.Helper()
	conf := "hostname: wolf\ntags: dld-managed\nlxc.console.logfile: /var/lib/docker-lxc-daemon/logs/" + id + ".log\n"
	if err := os.WriteFile(filepath.Join(dir, "lxc", strconv.Itoa(vmid)+".conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeTools puts lxc-info (printing state) and a pct that logs its arguments to
// the returned file, exiting with pctRC and printing pctOut, first on PATH.
func fakeTools(t *testing.T, state, pctOut, pctRC string) string {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "pct-calls")
	if err := os.WriteFile(filepath.Join(bin, "lxc-info"), []byte("#!/bin/sh\necho "+state+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pct := "#!/bin/sh\necho \"$*\" >> \"$PCT_CALLS\"\nprintf '%s' \"$PCT_OUT\"\nexit \"$PCT_RC\"\n"
	if err := os.WriteFile(filepath.Join(bin, "pct"), []byte(pct), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PCT_CALLS", calls)
	t.Setenv("PCT_OUT", pctOut)
	t.Setenv("PCT_RC", pctRC)
	return calls
}

func pctCalls(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func newStoreWith(t *testing.T, recs ...*store.ContainerRecord) *store.Store {
	t.Helper()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	for _, rec := range recs {
		if err := st.AddContainer(rec); err != nil {
			t.Fatalf("AddContainer: %v", err)
		}
	}
	return st
}

func TestPVECTGone(t *testing.T) {
	dir := fakePVE(t)
	writeCT(t, dir, 104, vanishedID)
	writeCT(t, dir, 105, otherID)

	cases := []struct {
		name string
		rec  *store.ContainerRecord
		want bool
	}{
		{"its own CT", &store.ContainerRecord{ID: vanishedID, VMID: 104}, false},
		{"CT destroyed", &store.ContainerRecord{ID: vanishedID, VMID: 106}, true},
		{"id now another container's CT", &store.ContainerRecord{ID: vanishedID, VMID: 105}, true},
		{"not a Proxmox CT", &store.ContainerRecord{ID: vanishedID}, false},
		{"no record", nil, false},
	}
	for _, tc := range cases {
		if got := pveCTGone(tc.rec); got != tc.want {
			t.Errorf("%s: pveCTGone = %v, want %v", tc.name, got, tc.want)
		}
	}

	// With pmxcfs down every config is missing; that must not read as gone.
	if err := os.Remove(filepath.Join(dir, ".version")); err != nil {
		t.Fatal(err)
	}
	if pveCTGone(&store.ContainerRecord{ID: vanishedID, VMID: 106}) {
		t.Error("pmxcfs down: a record read as gone")
	}
}

func TestPctConfigMissing(t *testing.T) {
	if !pctConfigMissing([]byte("Configuration file 'nodes/pvetest/lxc/104.conf' does not exist\n")) {
		t.Error("pct's missing-config error not recognized")
	}
	if pctConfigMissing([]byte("CT is locked (create)")) {
		t.Error("an unrelated pct error read as a missing config")
	}
}

// The bug this guards: a CT destroyed behind the daemon's back left a record the
// restart watcher tried to `pct start` every 5 s forever, and compose kept
// trying to start it instead of creating a fresh container.
func TestRestartWatcherDropsContainerWhoseCTIsGone(t *testing.T) {
	fakePVE(t)
	calls := fakeTools(t, "STOPPED", "", "0")
	started := time.Now().Add(-time.Hour)
	st := newStoreWith(t, &store.ContainerRecord{
		ID: vanishedID, Name: "wolf", VMID: 104, StartedAt: &started, RestartPolicy: "unless-stopped",
	})
	mgr := &Manager{lxcPath: t.TempDir(), store: st}

	var events []string
	mgr.enforceRestartPolicy(vanishedID, func(id, action string) { events = append(events, action) })

	if got := st.GetContainer(vanishedID); got != nil {
		t.Fatalf("record of a destroyed CT kept: %+v", got)
	}
	if c := pctCalls(t, calls); c != "" {
		t.Fatalf("pct called for a destroyed CT: %q", c)
	}
	if len(events) != 1 || events[0] != "destroy" {
		t.Fatalf("events = %v, want [destroy]", events)
	}
}

func TestRestartWatcherKeepsRecordsWhilePmxcfsIsDown(t *testing.T) {
	dir := fakePVE(t)
	if err := os.Remove(filepath.Join(dir, ".version")); err != nil {
		t.Fatal(err)
	}
	fakeTools(t, "STOPPED", "", "0")
	started := time.Now().Add(-time.Hour)
	st := newStoreWith(t, &store.ContainerRecord{
		ID: vanishedID, Name: "wolf", VMID: 104, StartedAt: &started, RestartPolicy: "unless-stopped", StoppedByUser: true,
	})
	mgr := &Manager{lxcPath: t.TempDir(), store: st}
	mgr.enforceRestartPolicy(vanishedID, nil)
	if st.GetContainer(vanishedID) == nil {
		t.Fatal("record dropped while pmxcfs was down")
	}
}

// A record whose id another container's CT now holds must neither report that
// CT's state nor start or destroy it.
func TestForeignCTIsNotTouched(t *testing.T) {
	dir := fakePVE(t)
	writeCT(t, dir, 104, otherID)
	calls := fakeTools(t, "RUNNING", "", "0")
	st := newStoreWith(t, &store.ContainerRecord{ID: vanishedID, Name: "abc_wolf", VMID: 104})
	mgr := &Manager{lxcPath: t.TempDir(), store: st}

	if s, _ := mgr.State(vanishedID); s != "exited" {
		t.Errorf("State = %q, want exited (the running CT is another container's)", s)
	}
	if err := mgr.StartContainer(vanishedID); err == nil {
		t.Error("StartContainer started another container's CT")
	}
	if err := mgr.RemoveContainer(vanishedID); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if st.GetContainer(vanishedID) != nil {
		t.Error("record kept")
	}
	if c := pctCalls(t, calls); c != "" {
		t.Fatalf("pct called on another container's CT: %q", c)
	}
}

func TestRemoveContainerDropsRecordWhenCTVanishesDuringDestroy(t *testing.T) {
	dir := fakePVE(t)
	writeCT(t, dir, 104, vanishedID)
	fakeTools(t, "STOPPED", "Configuration file 'nodes/pve/lxc/104.conf' does not exist\n", "255")
	st := newStoreWith(t, &store.ContainerRecord{ID: vanishedID, Name: "wolf", VMID: 104})
	mgr := &Manager{lxcPath: t.TempDir(), store: st}

	if err := mgr.RemoveContainer(vanishedID); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if st.GetContainer(vanishedID) != nil {
		t.Error("record kept although its CT is gone")
	}
}

// A create whose pinned VMID the replaced container still holds builds its CT on
// first start. The config must come back intact, and a start that cannot build
// the CT yet must leave it pending for the next one.
func TestDeferredCreateBuildsTheCTOnStart(t *testing.T) {
	fakePVE(t)
	calls := fakeTools(t, "STOPPED", "", "0")
	st := newStoreWith(t, &store.ContainerRecord{ID: vanishedID, Name: "wolf", ImageID: "ghcr.io/example/wolf:stable"})
	mgr := &Manager{lxcPath: t.TempDir(), store: st}

	cfg := ContainerConfig{
		Env:           []string{"WOLF_PROTON_LOG=1"},
		Mounts:        []MountSpec{{Source: "/etc/wolf", Destination: "/etc/wolf"}},
		ProxmoxCT:     true,
		RequestedVMID: 104,
		DiskSizeGB:    50,
	}
	if err := mgr.DeferCreate(vanishedID, cfg); err != nil {
		t.Fatalf("DeferCreate: %v", err)
	}
	rec := st.GetContainer(vanishedID)
	var got ContainerConfig
	if err := json.Unmarshal(rec.PendingCreate, &got); err != nil {
		t.Fatalf("pending config: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("pending config = %+v, want %+v", got, cfg)
	}
	if c := pctCalls(t, calls); c != "" {
		t.Fatalf("DeferCreate ran pct: %q", c)
	}

	// The image is not in the store, so the create the start runs fails: the
	// start reports it and the container stays pending.
	err := mgr.StartContainer(vanishedID)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("StartContainer = %v, want the create's image error", err)
	}
	if len(st.GetContainer(vanishedID).PendingCreate) == 0 {
		t.Fatal("a failed deferred create dropped the pending config")
	}
}

// The success path: the first start builds the container from the config saved
// at create, clears the pending state, and starts it.
func TestDeferredCreateSucceedsOnStart(t *testing.T) {
	lxcPath := t.TempDir()
	template := "__template_example_wolf"
	if err := os.MkdirAll(filepath.Join(lxcPath, template, "rootfs", "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	startCalls := filepath.Join(t.TempDir(), "lxc-start-calls")
	if err := os.WriteFile(filepath.Join(bin, "lxc-info"), []byte("#!/bin/sh\necho STOPPED\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "lxc-start"), []byte("#!/bin/sh\necho \"$*\" >> \"$START_CALLS\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("START_CALLS", startCalls)

	st := newStoreWith(t, &store.ContainerRecord{ID: vanishedID, Name: "wolf", ImageID: "example/wolf:stable"})
	if err := st.AddImage(&store.ImageRecord{ID: "example_wolf", Ref: "example/wolf:stable", TemplateName: template}); err != nil {
		t.Fatal(err)
	}
	mgr := &Manager{lxcPath: lxcPath, store: st}

	if err := mgr.DeferCreate(vanishedID, ContainerConfig{NetworkMode: "host", Env: []string{"WOLF_PROTON_LOG=1"}}); err != nil {
		t.Fatalf("DeferCreate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lxcPath, vanishedID)); !os.IsNotExist(err) {
		t.Fatalf("DeferCreate built the container already (stat err %v)", err)
	}

	if err := mgr.StartContainer(vanishedID); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	conf, err := os.ReadFile(filepath.Join(lxcPath, vanishedID, "config"))
	if err != nil {
		t.Fatalf("container not built on start: %v", err)
	}
	if !strings.Contains(string(conf), "WOLF_PROTON_LOG=1") {
		t.Errorf("built container lacks the env saved at create:\n%s", conf)
	}
	if len(st.GetContainer(vanishedID).PendingCreate) != 0 {
		t.Error("pending config kept after a successful create")
	}
	if c := pctCalls(t, startCalls); !strings.Contains(c, "-n "+vanishedID) {
		t.Errorf("lxc-start calls = %q, want a start of the container", c)
	}

	// A second start must not create it again.
	if err := os.WriteFile(filepath.Join(lxcPath, vanishedID, "marker"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mgr.StartContainer(vanishedID); err != nil {
		t.Fatalf("second StartContainer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lxcPath, vanishedID, "marker")); err != nil {
		t.Error("second start re-created the container")
	}
}
