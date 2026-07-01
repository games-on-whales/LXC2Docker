package api

import "testing"

func TestRegistryHostFromAuth(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                              "docker.io",
		"docker.io":                     "docker.io",
		"index.docker.io":               "docker.io",
		"registry-1.docker.io":          "docker.io",
		"https://index.docker.io/v1/":   "docker.io",
		"http://index.docker.io/v1/":    "docker.io",
		"ghcr.io":                       "ghcr.io",
		"https://ghcr.io":               "ghcr.io",
		"registry.example.com:5000":     "registry.example.com:5000",
		"https://registry.example.com/": "registry.example.com",
		"  ghcr.io  ":                   "ghcr.io",
	}
	for in, want := range cases {
		if got := registryHostFromAuth(in); got != want {
			t.Errorf("registryHostFromAuth(%q) = %q, want %q", in, got, want)
		}
	}
}
