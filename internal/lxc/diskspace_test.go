package lxc

import (
	"testing"
)

// TestCheckCreateDiskPressureSkipsTmpfs covers the guardrail false positive that
// refused Wolf's PulseAudio sibling: its shared runtime dir is a small tmpfs, so
// FreeBytes reports little space, but a tmpfs cannot fill the host disk. /dev/shm
// is a tmpfs on a normal Linux host; skip the test where it is not.
func TestCheckCreateDiskPressureSkipsTmpfs(t *testing.T) {
	t.Parallel()

	const shm = "/dev/shm"
	if !isTmpfs(shm) {
		t.Skipf("%s is not tmpfs on this host", shm)
	}

	// Threshold far above any tmpfs free space; a non-tmpfs source would be
	// refused, but the tmpfs source must be skipped so create is allowed.
	m := &Manager{minFreeBytes: 1 << 50} // 1 PiB
	if err := m.checkCreateDiskPressure(ContainerConfig{
		Mounts: []MountSpec{{Source: shm}},
	}); err != nil {
		t.Fatalf("tmpfs bind source should not trip the guardrail: %v", err)
	}
}

func TestFreeBytes(t *testing.T) {
	t.Parallel()

	// A temp dir always exists on a real filesystem with some space free.
	free, err := FreeBytes(t.TempDir())
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}
	if free == 0 {
		t.Fatal("FreeBytes returned 0 for a writable temp dir")
	}

	// A non-existent path must error rather than report space.
	if _, err := FreeBytes("/nonexistent/path/should/not/exist"); err == nil {
		t.Fatal("FreeBytes on missing path: expected error, got nil")
	}
}

func TestCheckCreateDiskPressure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	free, err := FreeBytes(dir)
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}

	// Threshold above current free space → a writable bind source is rejected.
	m := &Manager{minFreeBytes: free + (1 << 40)} // free + 1TiB, guaranteed over
	if err := m.checkCreateDiskPressure(ContainerConfig{
		Mounts: []MountSpec{{Source: dir}},
	}); err == nil {
		t.Fatal("expected create to be refused when bind source is below threshold")
	}

	// Read-only sources are never blocked.
	if err := m.checkCreateDiskPressure(ContainerConfig{
		Mounts: []MountSpec{{Source: dir, ReadOnly: true}},
	}); err != nil {
		t.Fatalf("read-only source should not be blocked: %v", err)
	}

	// Zero threshold disables the check entirely.
	m0 := &Manager{minFreeBytes: 0}
	if err := m0.checkCreateDiskPressure(ContainerConfig{
		Mounts: []MountSpec{{Source: dir}},
	}); err != nil {
		t.Fatalf("zero threshold should disable the check: %v", err)
	}

	// A modest threshold the temp dir clears → allowed.
	mOK := &Manager{minFreeBytes: 1}
	if err := mOK.checkCreateDiskPressure(ContainerConfig{
		Mounts: []MountSpec{{Source: dir}},
	}); err != nil {
		t.Fatalf("expected create allowed when above threshold: %v", err)
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	cases := map[uint64]string{
		512:                     "512B",
		1024:                    "1.0KiB",
		1 << 20:                 "1.0MiB",
		1 << 30:                 "1.0GiB",
		3 * (1 << 30):           "3.0GiB",
		uint64(1.5 * (1 << 30)): "1.5GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
