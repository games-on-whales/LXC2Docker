package api

import (
	"reflect"
	"testing"
)

func TestSmoothNASLXCRawConfigOrdersNumericLabels(t *testing.T) {
	t.Parallel()

	got := smoothNASLXCRawConfig(map[string]string{
		"io.smoothnas.lxc.raw.2": "lxc.environment = THIRD=true",
		"io.smoothnas.lxc.raw.0": "lxc.mount.entry = /run/a run/a none bind,optional,create=dir 0 0",
		"io.smoothnas.lxc.raw.1": "lxc.environment = SECOND=true",
		"io.smoothnas.lxc.raw.x": "ignored",
		"other":                  "ignored",
	})
	want := []string{
		"lxc.mount.entry = /run/a run/a none bind,optional,create=dir 0 0",
		"lxc.environment = SECOND=true",
		"lxc.environment = THIRD=true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raw config = %#v, want %#v", got, want)
	}
}
