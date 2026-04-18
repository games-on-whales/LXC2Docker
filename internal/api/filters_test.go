package api

import (
	"net/http"
	"net/url"
	"testing"
)

func newRequestWithFilters(raw string) *http.Request {
	u := &url.URL{
		Scheme: "http",
		Host:   "example.com",
		Path:   "/v1",
	}
	values := u.Query()
	values.Set("filters", raw)
	u.RawQuery = values.Encode()

	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	return req
}

func TestParseFiltersJSONAndFallback(t *testing.T) {
	t.Parallel()

	req := newRequestWithFilters(`{"status":["running"],"label":["project=demo"]}`)
	got := parseFilters(req)
	if !got.matchAny("status", "running") {
		t.Fatal("expected running status filter to match")
	}
	if !got.matchLabel(map[string]string{"project": "demo"}) {
		t.Fatal("expected label filter to match")
	}

	req = newRequestWithFilters(`{"status":{"running":true,"stopped":false},"name":{"api":true}}`)
	got = parseFilters(req)
	if !got.matchAny("status", "running") {
		t.Fatal("expected fallback status filter to keep true values")
	}
	if got.matchAny("status", "stopped") {
		t.Fatal("expected fallback status filter to drop false values")
	}
}

func TestParseFiltersInvalidJSONFallsBackToEmptyMap(t *testing.T) {
	t.Parallel()

	got := parseFilters(newRequestWithFilters(`not-json`))
	if len(got) != 0 {
		t.Fatalf("expected empty filters on malformed JSON, got %#v", got)
	}
}

func TestParseFiltersMatchNameAndID(t *testing.T) {
	t.Parallel()

	f := filters{
		"name": []string{"/api"},
		"id":   []string{"abcd"},
	}
	if !f.matchNamePrefix([]string{"web", "/api-service"}) {
		t.Fatal("expected name filter to match substring")
	}
	if !f.matchID("abcdef1234") {
		t.Fatal("expected ID prefix filter to match")
	}
}

func TestMatchAnyAndLabels(t *testing.T) {
	t.Parallel()

	f := filters{
		"label": []string{"env=prod", "debug"},
		"type":  []string{"container"},
	}
	if !f.matchAny("type", "container") {
		t.Fatal("expected exact any-match")
	}
	if !f.matchLabel(map[string]string{
		"env":   "prod",
		"debug": "1",
	}) {
		t.Fatal("expected label filter to match")
	}
	if f.matchLabel(map[string]string{
		"env": "dev",
		"debug": "1",
	}) {
		t.Fatal("expected label filter mismatch")
	}
}
