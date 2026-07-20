package lxc

import "testing"

// TestProcRootRel covers the parser that recognizes a sibling-namespace bind
// source (/proc/<pid>/root/<rel>) and extracts its in-container relative path.
// This is the classifier refreshProcMountSources keys off before it decides a
// mount entry may have gone stale, so its edge cases matter.
func TestProcRootRel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		src     string
		wantRel string
		wantOK  bool
	}{
		{"typical", "/proc/4001235/root/var/lib/aimee", "var/lib/aimee", true},
		{"single-segment rel", "/proc/12/root/run", "run", true},
		{"deep rel", "/proc/9/root/a/b/c", "a/b/c", true},
		{"not proc", "/var/lib/aimee", "", false},
		{"proc but not root", "/proc/12/cwd/x", "", false},
		{"proc net (not a bind source)", "/proc/12/net/dev", "", false},
		{"non-numeric pid", "/proc/self/root/x", "", false},
		{"empty rel", "/proc/12/root/", "", false},
		{"root with no rel", "/proc/12/root", "", false},
		{"empty string", "", "", false},
		{"just /proc/", "/proc/", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel, ok := procRootRel(tc.src)
			if ok != tc.wantOK || rel != tc.wantRel {
				t.Errorf("procRootRel(%q) = (%q, %v), want (%q, %v)",
					tc.src, rel, ok, tc.wantRel, tc.wantOK)
			}
		})
	}
}
