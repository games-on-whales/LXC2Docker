package lxc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDiskSizeGB(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"   ", 0},
		{"garbage", 0},
		{"0", 0},
		{"-5G", 0},
		{"32", 32},      // bare number => GB
		{"32G", 32},
		{"32g", 32},
		{"32GB", 32},
		{"32GiB", 32},
		{" 64 G ", 64},
		{"1T", 1024},
		{"500M", 1},     // rounds up to whole GB
		{"1536M", 2},    // 1.5G rounds up
		{"1.5G", 2},     // fractional GB rounds up
		{"1073741824B", 1},
	}
	for _, tc := range cases {
		if got := ParseDiskSizeGB(tc.in); got != tc.want {
			t.Errorf("ParseDiskSizeGB(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestCapabilityItemsPrivileged(t *testing.T) {
	t.Parallel()

	items := capabilityItems(&ContainerConfig{Privileged: true})
	if len(items) != 2 {
		t.Fatalf("expected 2 capability items, got %d", len(items))
	}
	if items[0] != (configItem{key: "lxc.cap.drop", value: ""}) {
		t.Fatalf("unexpected first item: %#v", items[0])
	}
	if items[1] != (configItem{key: "lxc.cgroup2.devices.allow", value: "a"}) {
		t.Fatalf("unexpected second item: %#v", items[1])
	}
}

func TestCapabilityItemsUsesKeepListForCapAddAndDrop(t *testing.T) {
	t.Parallel()

	items := capabilityItems(&ContainerConfig{
		CapAdd:  []string{"NET_ADMIN"},
		CapDrop: []string{"NET_RAW"},
	})
	if len(items) == 0 {
		t.Fatal("expected capability items")
	}
	if items[0] != (configItem{key: "lxc.cap.drop", value: ""}) {
		t.Fatalf("expected inherited drops to be cleared first, got %#v", items[0])
	}

	var sawNetAdmin bool
	var sawNetRaw bool
	for _, item := range items[1:] {
		if item.key != "lxc.cap.keep" {
			t.Fatalf("expected keep-list entries, got %#v", item)
		}
		if item.value == "net_admin" {
			sawNetAdmin = true
		}
		if item.value == "net_raw" {
			sawNetRaw = true
		}
	}
	if !sawNetAdmin {
		t.Fatal("expected NET_ADMIN to be present in final keep-list")
	}
	if sawNetRaw {
		t.Fatal("expected NET_RAW to be removed from final keep-list")
	}
}

func TestNormalizeCap(t *testing.T) {
	t.Parallel()

	if got := normalizeCap(" CAP_NET_ADMIN "); got != "net_admin" {
		t.Fatalf("normalizeCap = %q, want %q", got, "net_admin")
	}
}

func TestAppendSocketMountMountsRuntimeSocketDirAtRealDestination(t *testing.T) {
	t.Parallel()

	runtimeDir := "/run/user/wolf"
	socket := "/run/user/wolf/wayland-1"

	cfg := &ContainerConfig{}
	items := appendSocketMount(nil, cfg, socket, MountSpec{
		Source:      socket,
		Destination: "/run/user/wolf/wayland-1",
	})

	want := strings.ReplaceAll(runtimeDir, " ", `\040`) + " run/user/wolf none bind,create=dir 0 0"
	if !hasMountEntry(items, want) {
		t.Fatalf("expected direct runtime dir mount %q, got %#v", want, items)
	}
	if len(cfg.SocketLinks) != 0 {
		t.Fatalf("expected no socket symlinks for direct runtime dir mount, got %#v", cfg.SocketLinks)
	}
}

func TestAppendSocketMountKeepsHiddenSocketMountForTranslatedDestinations(t *testing.T) {
	t.Parallel()

	runtimeDir := "/run/wolf"
	socket := "/run/wolf/wolf.sock"

	cfg := &ContainerConfig{}
	items := appendSocketMount(nil, cfg, socket, MountSpec{
		Source:      socket,
		Destination: "/var/run/wolf/wolf.sock",
	})

	want := strings.ReplaceAll(runtimeDir, " ", `\040`) + " .socket-dirs/wolf none bind,create=dir 0 0"
	if !hasMountEntry(items, want) {
		t.Fatalf("expected hidden socket dir mount %q, got %#v", want, items)
	}
	if got := cfg.SocketLinks["/var/run/wolf/wolf.sock"]; got != "/.socket-dirs/wolf/wolf.sock" {
		t.Fatalf("unexpected socket link target %q", got)
	}
}

func TestBuildItemsAppendsRawConfig(t *testing.T) {
	t.Parallel()

	items := buildItems(&ContainerConfig{
		RawConfig: []string{
			"lxc.mount.entry = /run/smoothnas-runtime/docker.sock var/run/docker.sock none bind,optional,create=file 0 0",
			"not-lxc = ignored",
			"lxc.environment = BAD=true\nlxc.environment = INJECTED=true",
		},
	}, "10.0.0.2")

	if !hasMountEntry(items, "/run/smoothnas-runtime/docker.sock var/run/docker.sock none bind,optional,create=file 0 0") {
		t.Fatalf("expected raw mount entry, got %#v", items)
	}
	for _, item := range items {
		if item.key == "not-lxc" || strings.Contains(item.value, "INJECTED") {
			t.Fatalf("unexpected raw item accepted: %#v", item)
		}
	}
}

func TestBuildItemsRewritesRawSocketMounts(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "docker.sock")
	oldStatUnixSocket := statUnixSocket
	statUnixSocket = func(path string) (string, bool) {
		return path, path == socketPath
	}
	t.Cleanup(func() { statUnixSocket = oldStatUnixSocket })

	cfg := &ContainerConfig{
		RawConfig: []string{
			"lxc.mount.entry = " + socketPath + " var/run/docker.sock none bind,optional,create=file 0 0",
		},
	}
	items := buildItems(cfg, "10.0.0.2")

	wantMount := strings.ReplaceAll(dir, " ", `\040`) + " .socket-dirs/" + filepath.Base(dir) + " none bind,create=dir 0 0"
	if !hasMountEntry(items, wantMount) {
		t.Fatalf("expected rewritten socket dir mount %q, got %#v", wantMount, items)
	}
	if hasMountEntry(items, socketPath+" var/run/docker.sock none bind,optional,create=file 0 0") {
		t.Fatalf("raw socket file mount should have been rewritten: %#v", items)
	}
	if got := cfg.SocketLinks["/var/run/docker.sock"]; got != "/.socket-dirs/"+filepath.Base(dir)+"/docker.sock" {
		t.Fatalf("unexpected socket link target %q", got)
	}
}

func TestBuildPVEItemsAppendsRawConfig(t *testing.T) {
	t.Parallel()

	items := buildPVEItems(&ContainerConfig{
		RawConfig: []string{
			"lxc.mount.entry = /dev/dri dev/dri none bind,optional,create=dir 0 0",
		},
	}, "10.0.0.2")

	if !hasMountEntry(items, "/dev/dri dev/dri none bind,optional,create=dir 0 0") {
		t.Fatalf("expected raw PVE mount entry, got %#v", items)
	}
}

func TestBuildItemsBindMountDefaultsToPrivatePropagation(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	items := buildItems(&ContainerConfig{
		Mounts: []MountSpec{{Source: src, Destination: "/dev"}},
	}, "10.0.0.2")

	want := strings.ReplaceAll(src, " ", `\040`) + " dev none bind,create=dir,rprivate 0 0"
	if !hasMountEntry(items, want) {
		t.Fatalf("expected rprivate bind mount %q, got %#v", want, items)
	}
}

func TestBuildItemsBindMountHonorsExplicitPropagation(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	items := buildItems(&ContainerConfig{
		Mounts: []MountSpec{{Source: src, Destination: "/data", Propagation: "rslave"}},
	}, "10.0.0.2")

	want := strings.ReplaceAll(src, " ", `\040`) + " data none bind,create=dir,rslave 0 0"
	if !hasMountEntry(items, want) {
		t.Fatalf("expected rslave bind mount %q, got %#v", want, items)
	}
}

func TestBuildPVEItemsBindMountDefaultsToPrivatePropagation(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	items := buildPVEItems(&ContainerConfig{
		Mounts: []MountSpec{{Source: src, Destination: "/dev"}},
	}, "10.0.0.2")

	want := strings.ReplaceAll(src, " ", `\040`) + " dev none bind,create=dir,rprivate 0 0"
	if !hasMountEntry(items, want) {
		t.Fatalf("expected rprivate bind mount %q, got %#v", want, items)
	}
}

// Device bind mounts (e.g. Wolf's /dev/dri, /dev/input, /dev/uinput) live under
// /dev just like a whole-/dev bind, so they must also be rprivate — otherwise a
// container that mounts over one of them could leak the mount back to the host
// and break it, the same failure mode the user-bind propagation fix closed.
func TestBuildItemsDeviceMountsArePrivate(t *testing.T) {
	t.Parallel()

	devDir := t.TempDir()             // bound as a device directory (create=dir)
	devNode := filepath.Join(devDir, "node")
	if err := os.WriteFile(devNode, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	items := buildItems(&ContainerConfig{
		Devices: []DeviceSpec{
			{PathOnHost: devDir, PathInContainer: "/dev/dri"},
			{PathOnHost: devNode, PathInContainer: "/dev/uinput"},
		},
	}, "10.0.0.2")

	wantDir := strings.ReplaceAll(devDir, " ", `\040`) + " dev/dri none bind,create=dir,rprivate 0 0"
	if !hasMountEntry(items, wantDir) {
		t.Fatalf("expected rprivate device dir mount %q, got %#v", wantDir, items)
	}
	wantNode := strings.ReplaceAll(devNode, " ", `\040`) + " dev/uinput none bind,create=file,rprivate 0 0"
	if !hasMountEntry(items, wantNode) {
		t.Fatalf("expected rprivate device node mount %q, got %#v", wantNode, items)
	}
}

func TestBuildPVEItemsDeviceMountsArePrivate(t *testing.T) {
	t.Parallel()

	devDir := t.TempDir()
	devNode := filepath.Join(devDir, "node")
	if err := os.WriteFile(devNode, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	items := buildPVEItems(&ContainerConfig{
		Devices: []DeviceSpec{
			{PathOnHost: devDir, PathInContainer: "/dev/dri"},
			{PathOnHost: devNode, PathInContainer: "/dev/uinput"},
		},
	}, "10.0.0.2")

	wantDir := strings.ReplaceAll(devDir, " ", `\040`) + " dev/dri none bind,create=dir,rprivate 0 0"
	if !hasMountEntry(items, wantDir) {
		t.Fatalf("expected rprivate device dir mount %q, got %#v", wantDir, items)
	}
	wantNode := strings.ReplaceAll(devNode, " ", `\040`) + " dev/uinput none bind,create=file,rprivate 0 0"
	if !hasMountEntry(items, wantNode) {
		t.Fatalf("expected rprivate device node mount %q, got %#v", wantNode, items)
	}
}

func TestMountPropagationOpt(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"":         "rprivate",
		"bogus":    "rprivate",
		"rslave":   "rslave",
		"shared":   "shared",
		"rprivate": "rprivate",
	}
	for in, want := range cases {
		if got := mountPropagationOpt(in); got != want {
			t.Fatalf("mountPropagationOpt(%q) = %q, want %q", in, got, want)
		}
	}
}

func hasMountEntry(items []configItem, want string) bool {
	for _, item := range items {
		if item.key == "lxc.mount.entry" && item.value == want {
			return true
		}
	}
	return false
}

func TestInitUserConfigItems(t *testing.T) {
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rootfs, "etc", "passwd"),
		[]byte("root:x:0:0:root:/root:/bin/bash\npostgres:x:999:1001:PostgreSQL:/var/lib/postgresql:/bin/bash\nadmin:x:1000:1000::/home/admin:/bin/sh\n"), 0o644)
	os.WriteFile(filepath.Join(rootfs, "etc", "group"),
		[]byte("root:x:0:\npostgres:x:1001:\nstaff:x:50:\n"), 0o644)

	asMap := func(items []configItem) map[string]string {
		m := map[string]string{}
		for _, it := range items {
			m[it.key] = it.value
		}
		return m
	}

	tests := []struct {
		name, spec  string
		wantUID     string // "" => expect no items at all
		wantGID     string // "" => expect lxc.init.gid absent
	}{
		{"empty", "", "", ""},
		{"numeric uid:gid", "1000:1000", "1000", "1000"},
		{"numeric uid in passwd -> primary gid", "1000", "1000", "1000"},
		{"numeric uid not in passwd -> no gid", "4242", "4242", ""},
		{"name resolves uid+primary gid", "postgres", "999", "1001"},
		{"name uid + name group", "postgres:staff", "999", "50"},
		{"name uid + numeric group override", "admin:0", "1000", "0"},
		{"unknown name -> nil", "nope", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := initUserConfigItems(tt.spec, rootfs)
			m := asMap(items)
			if tt.wantUID == "" {
				if len(items) != 0 {
					t.Fatalf("expected no items, got %v", items)
				}
				return
			}
			if m["lxc.init.uid"] != tt.wantUID {
				t.Fatalf("lxc.init.uid = %q, want %q (items=%v)", m["lxc.init.uid"], tt.wantUID, items)
			}
			if tt.wantGID == "" {
				if _, ok := m["lxc.init.gid"]; ok {
					t.Fatalf("expected no lxc.init.gid, got %q", m["lxc.init.gid"])
				}
			} else if m["lxc.init.gid"] != tt.wantGID {
				t.Fatalf("lxc.init.gid = %q, want %q (items=%v)", m["lxc.init.gid"], tt.wantGID, items)
			}
		})
	}
}

func TestContainerIDHostnameBind(t *testing.T) {
	t.Parallel()

	src := "/var/lib/docker-lxc-daemon/containers/" + strings.Repeat("a", 64) + "/hostname"
	want := src + " etc/hostname none bind,create=file 0 0"
	cfg := &ContainerConfig{IDHostnameSource: src}

	// Both build paths must emit the id-embedded /etc/hostname bind so apps can
	// self-identify from /proc/self/mountinfo (Docker parity).
	if !hasMountEntry(buildPVEItems(cfg, "10.0.0.2"), want) {
		t.Fatalf("buildPVEItems missing id-hostname bind %q", want)
	}
	if !hasMountEntry(buildItems(cfg, "10.0.0.2"), want) {
		t.Fatalf("buildItems missing id-hostname bind %q", want)
	}

	// With no source, no /etc/hostname bind is emitted.
	for _, it := range buildPVEItems(&ContainerConfig{}, "10.0.0.2") {
		if it.key == "lxc.mount.entry" && strings.Contains(it.value, " etc/hostname none bind") {
			t.Fatalf("unexpected /etc/hostname bind when IDHostnameSource empty: %q", it.value)
		}
	}
}
