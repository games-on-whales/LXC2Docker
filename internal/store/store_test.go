package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistenceAndLookup(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	s, err := NewAt(base)
	if err != nil {
		t.Fatalf("NewAt failed: %v", err)
	}

	if err := s.AddContainer(&ContainerRecord{
		ID:     "0123456789abcdef",
		Name:   "api-web",
		Labels: map[string]string{"project": "demo"},
	}); err != nil {
		t.Fatalf("AddContainer failed: %v", err)
	}
	if err := s.AddImage(&ImageRecord{
		ID:           "imgid",
		Ref:          "docker.io/library/nginx:latest",
		Created:      time.Time{},
		TemplateName: "tmpl",
	}); err != nil {
		t.Fatalf("AddImage failed: %v", err)
	}

	reloaded, err := NewAt(base)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if got := reloaded.GetContainer("0123456789abcdef"); got == nil {
		t.Fatal("container should persist")
	}
	if got := reloaded.ResolveID("/0123"); got != "0123456789abcdef" {
		t.Fatalf("expected ID prefix match, got %q", got)
	}
	if got := reloaded.GetImage("nginx:latest"); got == nil {
		t.Fatal("image should match by stripped docker.io/library prefix")
	}
}

func TestStoreResolveIDByName(t *testing.T) {
	t.Parallel()

	s, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt failed: %v", err)
	}
	if err := s.AddContainer(&ContainerRecord{
		ID:   "abcd",
		Name: "web",
	}); err != nil {
		t.Fatalf("AddContainer failed: %v", err)
	}

	for _, tc := range []struct {
		in   string
		want string
	}{
		{"abcd", "abcd"},
		{"/abcd", "abcd"},
		{"ab", "abcd"},
		{"web", "abcd"},
		{"missing", ""},
	} {
		if got := s.ResolveID(tc.in); got != tc.want {
			t.Fatalf("ResolveID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAllocateAndReuseIP(t *testing.T) {
	t.Parallel()

	s, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt failed: %v", err)
	}

	ip, err := s.AllocateIP()
	if err != nil || ip != "10.100.0.2" {
		t.Fatalf("first IP should be 10.100.0.2, got %q (%v)", ip, err)
	}
	ip, err = s.AllocateIP()
	if err != nil || ip != "10.100.0.3" {
		t.Fatalf("second IP should be 10.100.0.3, got %q (%v)", ip, err)
	}

	if err := s.AddContainer(&ContainerRecord{
		ID:        "manual",
		Name:      "manual",
		IPAddress: "10.100.0.200",
	}); err != nil {
		t.Fatalf("AddContainer failed: %v", err)
	}
	if err := s.RemoveContainer("manual"); err != nil {
		t.Fatalf("RemoveContainer failed: %v", err)
	}

	ip, err = s.AllocateIP()
	if err != nil || ip != "10.100.0.200" {
		t.Fatalf("expected freed IP to be reused, got %q (%v)", ip, err)
	}

	s.data.NextIP = 255
	if _, err := s.AllocateIP(); err == nil {
		t.Fatal("expected IP exhaustion when NextIP exceeds 254 and no free IPs remain")
	}
}

func TestAllocateIPSkipsAddressesStillInUse(t *testing.T) {
	t.Parallel()

	s, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt failed: %v", err)
	}
	if err := s.AddContainer(&ContainerRecord{
		ID:        "controller",
		Name:      "controller",
		IPAddress: "10.100.0.3",
	}); err != nil {
		t.Fatalf("AddContainer failed: %v", err)
	}

	s.data.FreeIPs = []int{3, 4}
	ip, err := s.AllocateIP()
	if err != nil {
		t.Fatalf("AllocateIP failed: %v", err)
	}
	if ip != "10.100.0.4" {
		t.Fatalf("expected allocator to skip in-use free-list IP, got %q", ip)
	}
	if len(s.data.FreeIPs) != 0 {
		t.Fatalf("expected used free-list entry to be scrubbed, got %v", s.data.FreeIPs)
	}
	if err := s.AddContainer(&ContainerRecord{
		ID:        "worker",
		Name:      "worker",
		IPAddress: ip,
	}); err != nil {
		t.Fatalf("AddContainer(worker) failed: %v", err)
	}

	s.data.NextIP = 3
	ip, err = s.AllocateIP()
	if err != nil {
		t.Fatalf("AllocateIP from counter failed: %v", err)
	}
	if ip != "10.100.0.5" {
		t.Fatalf("expected allocator to skip in-use counter IP, got %q", ip)
	}
}

func TestFreeIPDoesNotFreeAddressStillInUse(t *testing.T) {
	t.Parallel()

	s, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt failed: %v", err)
	}
	for _, rec := range []*ContainerRecord{
		{ID: "one", Name: "one", IPAddress: "10.100.0.6"},
		{ID: "two", Name: "two", IPAddress: "10.100.0.6"},
	} {
		if err := s.AddContainer(rec); err != nil {
			t.Fatalf("AddContainer failed: %v", err)
		}
	}

	s.FreeIP("10.100.0.6")
	if len(s.data.FreeIPs) != 0 {
		t.Fatalf("FreeIP should ignore addresses still in use, got %v", s.data.FreeIPs)
	}

	if err := s.RemoveContainer("one"); err != nil {
		t.Fatalf("RemoveContainer(one) failed: %v", err)
	}
	if len(s.data.FreeIPs) != 0 {
		t.Fatalf("RemoveContainer should not free duplicate address while still in use, got %v", s.data.FreeIPs)
	}

	if err := s.RemoveContainer("two"); err != nil {
		t.Fatalf("RemoveContainer(two) failed: %v", err)
	}
	if len(s.data.FreeIPs) != 1 || s.data.FreeIPs[0] != 6 {
		t.Fatalf("expected address to be freed after final user is removed, got %v", s.data.FreeIPs)
	}
}

func TestBareImageRef(t *testing.T) {
	t.Parallel()

	got := bareImageRef("docker.io/library/nginx:latest")
	if got != "nginx:latest" {
		t.Fatalf("expected stripped ref, got %q", got)
	}

	got = bareImageRef("registry:5000/library/app:tag")
	if filepath.Base(filepath.FromSlash(got)) != "app:tag" {
		t.Fatalf("unexpected registry strip behavior: %q", got)
	}
}
