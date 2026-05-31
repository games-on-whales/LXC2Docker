package lxc

import "testing"

func TestConfHasManagedTag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		conf string
		want bool
	}{
		{
			name: "sole managed tag",
			conf: "arch: amd64\nhostname: web\ntags: " + ManagedTag + "\n",
			want: true,
		},
		{
			name: "managed tag among others (semicolon-separated)",
			conf: "tags: prod;" + ManagedTag + ";gpu\n",
			want: true,
		},
		{
			name: "managed tag with surrounding whitespace",
			conf: "tags:   " + ManagedTag + "  \n",
			want: true,
		},
		{
			name: "no tags line at all",
			conf: "arch: amd64\nhostname: prod-db\n",
			want: false,
		},
		{
			name: "untagged production CT",
			conf: "tags: prod;postgres\n",
			want: false,
		},
		{
			name: "substring must not match (dld-managed-extra)",
			conf: "tags: " + ManagedTag + "-extra\n",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := confHasManagedTag([]byte(tc.conf)); got != tc.want {
				t.Fatalf("confHasManagedTag(%q) = %v, want %v", tc.conf, got, tc.want)
			}
		})
	}
}
