package api

import "testing"

// TestDockerStatus: every output must be one of Docker's seven valid
// State.Status values, and no LXC transient string may leak through.
func TestDockerStatus(t *testing.T) {
	t.Parallel()
	valid := map[string]bool{
		"created": true, "running": true, "paused": true, "restarting": true,
		"removing": true, "exited": true, "dead": true,
	}
	cases := map[string]string{
		// Docker states pass through unchanged.
		"created": "created", "running": "running", "paused": "paused",
		"restarting": "restarting", "removing": "removing", "exited": "exited", "dead": "dead",
		// LXC states the manager already maps, plus transients it doesn't.
		"frozen": "paused", "freezing": "paused",
		"thawed": "running", "starting": "running", "stopping": "running",
		"aborting": "exited",
		// Anything unexpected must not leak.
		"weird-lxc-state": "exited", "": "exited",
	}
	for in, want := range cases {
		got := dockerStatus(in)
		if got != want {
			t.Errorf("dockerStatus(%q) = %q, want %q", in, got, want)
		}
		if !valid[got] {
			t.Errorf("dockerStatus(%q) = %q, which is not a valid Docker State.Status", in, got)
		}
	}
}
