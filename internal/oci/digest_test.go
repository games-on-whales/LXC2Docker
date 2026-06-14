package oci

import "testing"

func TestIsDigestPinned(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{"nginx:latest", false},
		{"ghcr.io/org/app:v1", false},
		{"ghcr.io/org/app", false},
		{"nginx@sha256:0000000000000000000000000000000000000000000000000000000000000000", true},
		{"ghcr.io/org/app:v1@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", true},
	}
	for _, c := range cases {
		if got := IsDigestPinned(c.ref); got != c.want {
			t.Errorf("IsDigestPinned(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}
