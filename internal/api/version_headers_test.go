package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestVersionHeadersOnEveryResponse: Docker sets the version-negotiation
// headers on every response, not just /_ping. Exercise a normal endpoint
// (/version) through the full router and confirm the middleware set them.
func TestVersionHeadersOnEveryResponse(t *testing.T) {
	t.Parallel()
	h := &Handler{}
	srv := h.routes()

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest("GET", "/version", nil))

	if rr.Code != 200 {
		t.Fatalf("/version status = %d, want 200", rr.Code)
	}
	for k, want := range map[string]string{
		"Api-Version":         "1.43",
		"Ostype":              "linux",
		"Docker-Experimental": "false",
	} {
		if got := rr.Header().Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if s := rr.Header().Get("Server"); !strings.HasPrefix(s, "Docker/") {
		t.Errorf("Server header = %q, want a Docker/... value", s)
	}

	// Docker sets the headers even on unmatched-route 404s. Wrapping the whole
	// router (not r.Use, which skips NotFoundHandler) must cover this.
	rr404 := httptest.NewRecorder()
	srv.ServeHTTP(rr404, httptest.NewRequest("GET", "/no/such/route", nil))
	if rr404.Code != 404 {
		t.Fatalf("unknown route status = %d, want 404", rr404.Code)
	}
	if got := rr404.Header().Get("Api-Version"); got != "1.43" {
		t.Errorf("404 Api-Version = %q, want 1.43 (headers must cover unmatched routes)", got)
	}
}
