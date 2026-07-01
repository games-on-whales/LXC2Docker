package api

import (
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// TestBuildHostConfigPreservesHostIP: a non-default HostIp on a port binding
// must survive into inspect's HostConfig.PortBindings (Portainer "Duplicate"
// re-posts inspect output, so losing it would default the port to all
// interfaces). An empty HostIp renders as "0.0.0.0" like Docker.
func TestBuildHostConfigPreservesHostIP(t *testing.T) {
	t.Parallel()
	rec := &store.ContainerRecord{
		ID: "abcdef0123456789",
		PortBindings: []store.PortBinding{
			// Two host IPs for the SAME container port must both survive under
			// the one "80/tcp" key, each keeping its own HostIp.
			{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Proto: "tcp"},
			{HostIP: "192.168.1.5", HostPort: 8081, ContainerPort: 80, Proto: "tcp"},
			{HostIP: "", HostPort: 9090, ContainerPort: 90, Proto: "tcp"},
		},
	}
	hc := buildHostConfig(rec)

	got80 := hc.PortBindings["80/tcp"]
	if len(got80) != 2 {
		t.Fatalf("80/tcp = %+v, want two bindings", got80)
	}
	if got80[0].HostIP != "127.0.0.1" || got80[0].HostPort != "8080" ||
		got80[1].HostIP != "192.168.1.5" || got80[1].HostPort != "8081" {
		t.Fatalf("80/tcp = %+v, want [127.0.0.1:8080, 192.168.1.5:8081]", got80)
	}
	got90 := hc.PortBindings["90/tcp"]
	if len(got90) != 1 || got90[0].HostIP != "0.0.0.0" {
		t.Fatalf("90/tcp = %+v, want HostIp 0.0.0.0 for empty binding", got90)
	}
}
