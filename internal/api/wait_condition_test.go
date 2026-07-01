package api

import "testing"

func TestValidWaitCondition(t *testing.T) {
	t.Parallel()
	valid := []string{"not-running", "next-exit", "removed"}
	for _, c := range valid {
		if !validWaitCondition(c) {
			t.Errorf("validWaitCondition(%q) = false, want true", c)
		}
	}
	invalid := []string{"", "running", "exited", "not_running", "NextExit", "foo", "next-exit "}
	for _, c := range invalid {
		if validWaitCondition(c) {
			t.Errorf("validWaitCondition(%q) = true, want false", c)
		}
	}
}
