package api

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

func TestStreamContentType(t *testing.T) {
	t.Parallel()
	const raw = "application/vnd.docker.raw-stream"
	const mux42 = "application/vnd.docker.multiplexed-stream"

	cases := []struct {
		version string
		tty     bool
		want    string
	}{
		{"", false, mux42},     // no version → current daemon → multiplexed
		{"", true, raw},        // tty always raw
		{"1.43", false, mux42}, // >= 1.42
		{"1.42", false, mux42}, // exactly 1.42
		{"1.41", false, raw},   // < 1.42 → raw (predates the media type)
		{"1.24", false, raw},   // old client
		{"1.41", true, raw},    // tty always raw regardless of version
		{"1.50", false, mux42}, // future
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/x", nil)
		if tc.version != "" {
			r = mux.SetURLVars(r, map[string]string{"version": tc.version})
		}
		if got := streamContentType(r, tc.tty); got != tc.want {
			t.Errorf("streamContentType(version=%q, tty=%v) = %q, want %q", tc.version, tc.tty, got, tc.want)
		}
	}
}

func TestCompareDottedVersions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want int
	}{
		{"1.42", "1.42", 0},
		{"1.43", "1.42", 1},
		{"1.41", "1.42", -1},
		{"1.9", "1.10", -1}, // numeric, not lexicographic
		{"2.0", "1.99", 1},
		{"1.42", "1.42.0", 0},
		{"1", "1.0", 0},
	}
	for _, tc := range cases {
		if got := compareDottedVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareDottedVersions(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestWriteHijackPreamble(t *testing.T) {
	t.Parallel()
	const ctype = "application/vnd.docker.multiplexed-stream"
	cases := []struct {
		name        string
		upgrade     bool
		wantStatus  string
		wantUpgrade bool // expect Connection/Upgrade headers
	}{
		{"upgrade", true, "HTTP/1.1 101 UPGRADED", true},
		{"no-upgrade", false, "HTTP/1.1 200 OK", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeHijackPreamble(&buf, tc.upgrade, ctype)
			out := buf.String()

			if !strings.HasPrefix(out, tc.wantStatus+"\r\n") {
				t.Errorf("status line = %q, want prefix %q", firstLine(out), tc.wantStatus)
			}
			if !strings.Contains(out, "Content-Type: "+ctype+"\r\n") {
				t.Errorf("missing Content-Type %q in:\n%q", ctype, out)
			}
			hasUpgrade := strings.Contains(out, "Connection: Upgrade\r\n") && strings.Contains(out, "Upgrade: tcp\r\n")
			if hasUpgrade != tc.wantUpgrade {
				t.Errorf("Connection/Upgrade headers present = %v, want %v (out=%q)", hasUpgrade, tc.wantUpgrade, out)
			}
			// A non-upgrading response must NOT carry Upgrade headers.
			if !tc.wantUpgrade && strings.Contains(out, "Upgrade: tcp") {
				t.Errorf("200 response leaked Upgrade header: %q", out)
			}
			if !strings.HasSuffix(out, "\r\n\r\n") {
				t.Errorf("preamble not terminated by blank line: %q", out)
			}
		})
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
