package api

import (
	"reflect"
	"testing"
)

func TestEnsureDefaultPath(t *testing.T) {
	t.Parallel()
	def := ociDefaultPath

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty gets default", nil, []string{def}},
		{"no PATH gets default prepended, others kept",
			[]string{"LANG=C", "FOO=bar"}, []string{def, "LANG=C", "FOO=bar"}},
		{"existing PATH untouched",
			[]string{"PATH=/custom/bin", "LANG=C"}, []string{"PATH=/custom/bin", "LANG=C"}},
		{"explicit empty PATH respected (not overridden)",
			[]string{"PATH="}, []string{"PATH="}},
		{"GOPATH is not PATH — default still injected",
			[]string{"GOPATH=/go"}, []string{def, "GOPATH=/go"}},
	}
	for _, tc := range cases {
		got := ensureDefaultPath(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: ensureDefaultPath(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}
