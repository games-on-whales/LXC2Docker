package lxc

import "testing"

func TestParseMemorySize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"  ", 0, false},
		{"16G", 16 << 30, false},
		{"16g", 16 << 30, false},
		{"16GiB", 16 << 30, false},
		{"16GB", 16 << 30, false},
		{"32768M", 32 << 30, false},
		{"512K", 512 << 10, false},
		{"1T", 1 << 40, false},
		{"1048576", 1048576, false}, // plain bytes
		{" 8G ", 8 << 30, false},
		{"abc", 0, true},
		{"16Q", 0, true},
		{"-4G", 0, true},
		{"G", 0, true},
	}
	for _, c := range cases {
		got, err := ParseMemorySize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseMemorySize(%q): expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMemorySize(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMemorySize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
