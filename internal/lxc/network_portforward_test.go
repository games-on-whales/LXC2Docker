package lxc

import (
	"reflect"
	"strings"
	"testing"
)

// A representative "nft -a list chain ip veth_nat prerouting" listing with two
// DNAT rules for the same host port — one stale (old container IP, left behind
// by a restart) and one fresh — plus an unrelated rule. The self-healing
// AddPortForward must target BOTH 47989 rules so the stale one can't shadow the
// new one, while leaving the unrelated 47984 rule alone.
const sampleChainListing = `table ip veth_nat {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		tcp dport 47989 dnat to 10.100.0.22:47989 # handle 7
		tcp dport 47989 dnat to 10.100.0.23:47989 # handle 12
		tcp dport 47984 dnat to 10.100.0.23:47984 # handle 13
		tcp dport 479890 dnat to 10.100.0.23:479890 # handle 14
	}
}`

func TestNATLineMatchesHostPort(t *testing.T) {
	cases := []struct {
		line     string
		hostPort int
		proto    string
		want     bool
	}{
		{"\t\ttcp dport 47989 dnat to 10.100.0.22:47989 # handle 7", 47989, "tcp", true},
		// Trailing-space boundary: 47989 must not match 479890 or 147989.
		{"\t\ttcp dport 479890 dnat to 10.100.0.23:479890 # handle 14", 47989, "tcp", false},
		{"\t\ttcp dport 147989 dnat to 10.100.0.23:147989 # handle 15", 47989, "tcp", false},
		// Proto must match.
		{"\t\tudp dport 47989 dnat to 10.100.0.22:47989 # handle 7", 47989, "tcp", false},
		// A non-DNAT line on the same port (e.g. a masquerade/accept) is not ours.
		{"\t\ttcp dport 47989 accept # handle 9", 47989, "tcp", false},
	}
	for _, c := range cases {
		if got := natLineMatchesHostPort(c.line, c.hostPort, c.proto); got != c.want {
			t.Errorf("natLineMatchesHostPort(%q, %d, %q) = %v, want %v", c.line, c.hostPort, c.proto, got, c.want)
		}
	}
}

func TestNATHandlesToDelete_HostPortEvictsStaleAndFresh(t *testing.T) {
	got := natHandlesToDelete(sampleChainListing, func(line string) bool {
		return natLineMatchesHostPort(line, 47989, "tcp")
	})
	want := []string{"7", "12"} // both 47989 rules; NOT 47984 (13) or 479890 (14)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("handles to delete for 47989/tcp = %v, want %v", got, want)
	}
}

func TestNATHandlesToDelete_ByContainerIP(t *testing.T) {
	// RemovePortForwards' predicate: every rule targeting one container IP.
	got := natHandlesToDelete(sampleChainListing, func(line string) bool {
		return strings.Contains(line, "dnat to 10.100.0.23:")
	})
	want := []string{"12", "13", "14"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("handles for IP 10.100.0.23 = %v, want %v", got, want)
	}
}
