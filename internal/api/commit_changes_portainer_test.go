package api

import (
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestApplyCommitChangesSupportsPortainerCommitInstructions(t *testing.T) {
	t.Parallel()

	rec := &store.ImageRecord{
		OCIEnv:        []string{"PATH=/bin"},
		OCIWorkingDir: "/base",
		OCIPorts:      []string{"80/tcp"},
		OCILabels:     map[string]string{"base": "1"},
		OCIVolumes:    []string{"/existing"},
		OCIHealthcheck: &store.HealthcheckConfig{
			Test: []string{"CMD", "true"},
		},
	}

	err := applyCommitChanges(rec, []string{
		"ENV A=B C=D",
		"LABEL app=demo role=web",
		"USER 1000:1000",
		"WORKDIR app",
		`CMD ["serve"]`,
		`ENTRYPOINT ["/entry"]`,
		"EXPOSE 8080/tcp 8443/tcp",
		`VOLUME ["/data","/cache"]`,
		"STOPSIGNAL SIGQUIT",
		`HEALTHCHECK --interval=30s CMD ["/bin/true"]`,
		`SHELL ["/bin/bash","-c"]`,
	})
	if err != nil {
		t.Fatalf("apply commit changes: %v", err)
	}

	if rec.OCIUser != "1000:1000" {
		t.Fatalf("expected OCIUser override, got %q", rec.OCIUser)
	}
	if rec.OCIWorkingDir != "/base/app" {
		t.Fatalf("expected relative WORKDIR to resolve, got %q", rec.OCIWorkingDir)
	}
	if len(rec.OCICmd) != 1 || rec.OCICmd[0] != "serve" {
		t.Fatalf("expected CMD override, got %#v", rec.OCICmd)
	}
	if len(rec.OCIEntrypoint) != 1 || rec.OCIEntrypoint[0] != "/entry" {
		t.Fatalf("expected ENTRYPOINT override, got %#v", rec.OCIEntrypoint)
	}
	if len(rec.OCIEnv) != 3 {
		t.Fatalf("expected merged env, got %#v", rec.OCIEnv)
	}
	if rec.OCILabels["app"] != "demo" || rec.OCILabels["role"] != "web" || rec.OCILabels["base"] != "1" {
		t.Fatalf("expected merged labels, got %#v", rec.OCILabels)
	}
	if len(rec.OCIPorts) != 3 {
		t.Fatalf("expected merged exposed ports, got %#v", rec.OCIPorts)
	}
	if len(rec.OCIVolumes) != 3 {
		t.Fatalf("expected merged volumes, got %#v", rec.OCIVolumes)
	}
	if rec.OCIStopSignal != "SIGQUIT" {
		t.Fatalf("expected stop signal override, got %q", rec.OCIStopSignal)
	}
	if rec.OCIHealthcheck == nil || len(rec.OCIHealthcheck.Test) != 2 || rec.OCIHealthcheck.Test[1] != "/bin/true" {
		t.Fatalf("expected healthcheck override, got %#v", rec.OCIHealthcheck)
	}
	if len(rec.OCIShell) != 2 || rec.OCIShell[0] != "/bin/bash" {
		t.Fatalf("expected shell override, got %#v", rec.OCIShell)
	}
}

func TestApplyCommitChangesSupportsPortainerHealthcheckDisableAndShellForms(t *testing.T) {
	t.Parallel()

	rec := &store.ImageRecord{
		OCIWorkingDir: "/work",
		OCIHealthcheck: &store.HealthcheckConfig{
			Test: []string{"CMD", "true"},
		},
	}

	err := applyCommitChanges(rec, []string{
		"WORKDIR child",
		"CMD echo hi",
		"ENTRYPOINT run-app",
		"VOLUME /logs /tmp",
		"HEALTHCHECK NONE",
	})
	if err != nil {
		t.Fatalf("apply commit changes: %v", err)
	}

	if rec.OCIWorkingDir != "/work/child" {
		t.Fatalf("expected relative WORKDIR to resolve, got %q", rec.OCIWorkingDir)
	}
	if len(rec.OCICmd) != 3 || rec.OCICmd[0] != "/bin/sh" {
		t.Fatalf("expected shell-form CMD parsing, got %#v", rec.OCICmd)
	}
	if len(rec.OCIEntrypoint) != 3 || rec.OCIEntrypoint[0] != "/bin/sh" {
		t.Fatalf("expected shell-form ENTRYPOINT parsing, got %#v", rec.OCIEntrypoint)
	}
	if len(rec.OCIVolumes) != 2 {
		t.Fatalf("expected whitespace VOLUME parsing, got %#v", rec.OCIVolumes)
	}
	if rec.OCIHealthcheck == nil || len(rec.OCIHealthcheck.Test) != 1 || rec.OCIHealthcheck.Test[0] != "NONE" {
		t.Fatalf("expected healthcheck disable, got %#v", rec.OCIHealthcheck)
	}
}

func TestParseCommitChangeInstructionsSupportsRepeatedAndMultilinePortainerChanges(t *testing.T) {
	t.Parallel()

	instrs, err := parseCommitChangeInstructions([]string{
		"ENV A=B\nLABEL app=demo",
		"WORKDIR /srv",
	})
	if err != nil {
		t.Fatalf("parse commit changes: %v", err)
	}
	if len(instrs) != 3 {
		t.Fatalf("expected 3 commit instructions, got %#v", instrs)
	}
	if instrs[0].op != "ENV" || instrs[1].op != "LABEL" || instrs[2].op != "WORKDIR" {
		t.Fatalf("unexpected instruction sequence %#v", instrs)
	}
}
