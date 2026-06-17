package api

import (
	"strings"
	"testing"
)

// Regression for "go: not found" when building FROM golang (and any base whose
// tools live outside the default PATH): the RUN environment is built by
// mergeEnv(daemonEnv, stageEnv), so the stage/base-image env must WIN over the
// daemon's and there must be exactly one PATH= entry. The previous
// append(os.Environ(), state.env...) left two PATH= entries with the daemon's
// first; glibc getenv reads the first, hiding /usr/local/go/bin.
func TestRunEnvImagePathWinsOverDaemon(t *testing.T) {
	daemonEnv := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
	}
	// golang:1.25-bookworm-style config env.
	stageEnv := []string{
		"PATH=/go/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"GOPATH=/go",
		"GOLANG_VERSION=1.25.11",
	}

	got := mergeEnv(daemonEnv, stageEnv)

	pathCount := 0
	for _, e := range got {
		if strings.HasPrefix(e, "PATH=") {
			pathCount++
		}
	}
	if pathCount != 1 {
		t.Fatalf("RUN env must have exactly one PATH= entry, got %d: %v", pathCount, got)
	}
	if p := envValue(got, "PATH"); !strings.Contains(p, "/usr/local/go/bin") {
		t.Errorf("image PATH must win so go is on PATH; got PATH=%q", p)
	}
	if got := envValue(got, "GOPATH"); got != "/go" {
		t.Errorf("base image GOPATH dropped: got %q", got)
	}
}
