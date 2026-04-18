package api

import (
	"strconv"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestParseListFiltersSliceAndMapForm(t *testing.T) {
	t.Parallel()

	got, err := parseListFilters(`{"status":["running","exited"],"label":{"app":true,"debug":false}}`)
	if err != nil {
		t.Fatalf("parseListFilters returned error: %v", err)
	}
	if len(got["status"]) != 2 || got["status"][0] != "running" {
		t.Fatalf("unexpected status filter: %#v", got["status"])
	}
	if len(got["label"]) != 2 {
		t.Fatalf("expected label keys to be collected regardless of bool value, got %#v", got["label"])
	}

	got, err = parseListFilters(`{"status":{"running":true,"exited":false}}`)
	if err != nil {
		t.Fatalf("parseListFilters returned error: %v", err)
	}
	if len(got["status"]) != 1 || got["status"][0] != "running" {
		t.Fatalf("unexpected map-form status filter: %#v", got["status"])
	}

	got, err = parseListFilters("")
	if err != nil {
		t.Fatalf("parseListFilters returned error for empty input: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map for empty filters input, got %#v", got)
	}
}

func TestParsePruneUntil(t *testing.T) {
	t.Parallel()

	ts := time.Date(2025, 2, 1, 10, 0, 0, 0, time.UTC)
	f := listFilters{"until": []string{ts.Format(time.RFC3339)}}
	got, err := parsePruneUntil(f)
	if err != nil {
		t.Fatalf("parsePruneUntil returned error: %v", err)
	}
	if !got.Equal(ts) {
		t.Fatalf("expected %s, got %s", ts.Format(time.RFC3339), got.Format(time.RFC3339))
	}

	epoch := time.Unix(1_700_000_000, 0).UTC()
	f = listFilters{"until": []string{strconv.FormatInt(epoch.Unix(), 10)}}
	gotUnix, err := parsePruneUntil(f)
	if err != nil {
		t.Fatalf("parsePruneUntil epoch failed: %v", err)
	}
	if !gotUnix.Equal(epoch) {
		t.Fatalf("expected epoch %s, got %s", epoch.Format(time.RFC3339), gotUnix.Format(time.RFC3339))
	}

	f = listFilters{"until": []string{epoch.Format(time.UnixDate)}}
	if got, err := parsePruneUntil(f); err == nil {
		// The UnUnixDate format must be rejected.
		t.Fatalf("expected invalid until format to fail, got %s", got.Format(time.RFC3339))
	}
}

func TestParsePruneUntilInvalid(t *testing.T) {
	t.Parallel()

	_, err := parsePruneUntil(listFilters{"until": []string{"not-a-timestamp"}})
	if err == nil {
		t.Fatal("expected invalid until value to fail")
	}
}

func TestMatchesContainerFilters(t *testing.T) {
	t.Parallel()

	rec := &store.ContainerRecord{
		ID:     "0123456789abcdef",
		Name:   "web",
		Image:  "docker.io/library/nginx:latest",
		Labels: map[string]string{"project": "demo", "team": "qa"},
	}

	f := listFilters{
		"status":   []string{"running"},
		"id":       []string{"0123"},
		"name":     []string{"web"},
		"ancestor": []string{"nginx"},
		"label":    []string{"project=demo"},
	}
	if !matchesContainerFilters(rec, "running", f) {
		t.Fatal("expected container filters to match")
	}

	f = listFilters{
		"status": []string{"exited"},
	}
	if matchesContainerFilters(rec, "running", f) {
		t.Fatal("expected status mismatch")
	}
}

func TestMatchesVolumeFilters(t *testing.T) {
	t.Parallel()

	volume := &store.VolumeRecord{
		Name:   "dbdata",
		Driver: "",
		Labels: map[string]string{"project": "demo"},
	}
	if !matchesVolumeFilters(volume, 1, listFilters{
		"driver":   []string{"local"},
		"name":     []string{"dbdata"},
		"label":    []string{"project=demo"},
		"dangling": []string{"false"},
	}) {
		t.Fatal("expected matching non-dangling volume")
	}
	if matchesVolumeFilters(volume, 1, listFilters{
		"driver":   []string{"local"},
		"name":     []string{"dbdata"},
		"label":    []string{"project=demo"},
		"dangling": []string{"true"},
	}) {
		t.Fatal("expected dangling filter mismatch")
	}
}
