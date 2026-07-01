package api

import (
	"net/http/httptest"
	"testing"
)

func TestBoolValue(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":        false, // absent / empty
		"0":       false,
		"false":   false,
		"FALSE":   false,
		"False":   false,
		"no":      false,
		"NO":      false,
		"none":    false,
		"None":    false,
		"1":       true,
		"true":    true,
		"True":    true,
		"TRUE":    true,
		"yes":     true,
		"on":      true,
		"anytext": true,
		"2":       true,
		" true ":  true, // trimmed
	}
	for raw, want := range cases {
		r := httptest.NewRequest("GET", "/x", nil)
		q := r.URL.Query()
		q.Set("force", raw)
		r.URL.RawQuery = q.Encode()
		if got := boolValue(r, "force"); got != want {
			t.Errorf("boolValue(force=%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestBoolValueAbsent(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/x", nil)
	if boolValue(r, "force") {
		t.Errorf("absent param should be false")
	}
}

func TestBoolValueDefault(t *testing.T) {
	t.Parallel()
	// Absent → default.
	r := httptest.NewRequest("GET", "/x", nil)
	if got := boolValueDefault(r, "stream", true); got != true {
		t.Errorf("absent stream with default true = %v, want true", got)
	}
	if got := boolValueDefault(r, "stream", false); got != false {
		t.Errorf("absent stream with default false = %v, want false", got)
	}
	// Present → parsed, ignoring default.
	for raw, want := range map[string]bool{"0": false, "false": false, "1": true, "yes": true} {
		r := httptest.NewRequest("GET", "/x", nil)
		q := r.URL.Query()
		q.Set("stream", raw)
		r.URL.RawQuery = q.Encode()
		if got := boolValueDefault(r, "stream", true); got != want {
			t.Errorf("boolValueDefault(stream=%q, def=true) = %v, want %v", raw, got, want)
		}
	}
}
