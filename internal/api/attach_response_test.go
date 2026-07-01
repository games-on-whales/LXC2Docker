package api

import (
	"bytes"
	"strings"
	"testing"
)

func TestStreamContentType(t *testing.T) {
	t.Parallel()
	if got := streamContentType(true); got != "application/vnd.docker.raw-stream" {
		t.Errorf("tty content-type = %q, want raw-stream", got)
	}
	if got := streamContentType(false); got != "application/vnd.docker.multiplexed-stream" {
		t.Errorf("non-tty content-type = %q, want multiplexed-stream", got)
	}
}

func TestWriteHijackPreamble(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		upgrade     bool
		tty         bool
		wantStatus  string
		wantCType   string
		wantUpgrade bool // expect Connection/Upgrade headers
	}{
		{"upgrade non-tty", true, false, "HTTP/1.1 101 UPGRADED", "application/vnd.docker.multiplexed-stream", true},
		{"upgrade tty", true, true, "HTTP/1.1 101 UPGRADED", "application/vnd.docker.raw-stream", true},
		{"no-upgrade non-tty", false, false, "HTTP/1.1 200 OK", "application/vnd.docker.multiplexed-stream", false},
		{"no-upgrade tty", false, true, "HTTP/1.1 200 OK", "application/vnd.docker.raw-stream", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeHijackPreamble(&buf, tc.upgrade, tc.tty)
			out := buf.String()

			if !strings.HasPrefix(out, tc.wantStatus+"\r\n") {
				t.Errorf("status line = %q, want prefix %q", firstLine(out), tc.wantStatus)
			}
			if !strings.Contains(out, "Content-Type: "+tc.wantCType+"\r\n") {
				t.Errorf("missing Content-Type %q in:\n%q", tc.wantCType, out)
			}
			hasUpgrade := strings.Contains(out, "Connection: Upgrade\r\n") && strings.Contains(out, "Upgrade: tcp\r\n")
			if hasUpgrade != tc.wantUpgrade {
				t.Errorf("Connection/Upgrade headers present = %v, want %v (out=%q)", hasUpgrade, tc.wantUpgrade, out)
			}
			// A non-upgrading response must NOT carry Upgrade headers (they would
			// confuse a plain HTTP client that never asked to switch protocols).
			if !tc.wantUpgrade && strings.Contains(out, "Upgrade: tcp") {
				t.Errorf("200 response leaked Upgrade header: %q", out)
			}
			// Must terminate the header block with a blank line.
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
