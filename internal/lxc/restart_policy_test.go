package lxc

import (
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// shouldStartNeverStarted decides whether the restart watcher converges a
// never-started ("created") container to running. Only always/unless-stopped
// qualify; a never-started container hasn't failed (so on-failure is out) and
// "no"/"" means hands-off. This is what keeps an orchestrator's create+start
// race from stranding a service in "created" forever.
func TestShouldStartNeverStarted(t *testing.T) {
	cases := []struct {
		name    string
		policy  string
		stopped bool
		want    bool
	}{
		{"always starts", "always", false, true},
		{"always starts even if stopped flag set", "always", true, true},
		{"unless-stopped starts", "unless-stopped", false, true},
		{"unless-stopped honors stopped-by-user", "unless-stopped", true, false},
		{"on-failure does not start a never-run container", "on-failure", false, false},
		{"no policy is hands-off", "no", false, false},
		{"empty policy is hands-off", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &store.ContainerRecord{RestartPolicy: c.policy, StoppedByUser: c.stopped}
			if got := shouldStartNeverStarted(rec); got != c.want {
				t.Fatalf("shouldStartNeverStarted(policy=%q,stopped=%v)=%v want %v",
					c.policy, c.stopped, got, c.want)
			}
		})
	}
}

func TestShouldRestart(t *testing.T) {
	cases := []struct {
		name     string
		policy   string
		stopped  bool
		maxRetry int
		count    int
		want     bool
	}{
		{"always", "always", false, 0, 0, true},
		{"always ignores stopped-by-user", "always", true, 0, 0, true},
		{"unless-stopped", "unless-stopped", false, 0, 0, true},
		{"unless-stopped stopped-by-user", "unless-stopped", true, 0, 0, false},
		{"on-failure under cap", "on-failure", false, 3, 1, true},
		{"on-failure at cap", "on-failure", false, 3, 3, false},
		{"on-failure unlimited", "on-failure", false, 0, 99, true},
		{"no", "no", false, 0, 0, false},
		{"empty", "", false, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &store.ContainerRecord{
				RestartPolicy:   c.policy,
				StoppedByUser:   c.stopped,
				RestartMaxRetry: c.maxRetry,
				RestartCount:    c.count,
			}
			if got := shouldRestart(rec); got != c.want {
				t.Fatalf("shouldRestart(%+v)=%v want %v", c, got, c.want)
			}
		})
	}
}
