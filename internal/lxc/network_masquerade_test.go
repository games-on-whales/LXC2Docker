package lxc

import (
	"strings"
	"testing"
)

// EnsureBridge runs every 30 s; a script that only lists the rules appends
// them each time. The flush must come after the chain exists and before the
// rules, all in the one script nft applies as a single transaction.
func TestMasqueradeScriptFlushesBeforeAddingRules(t *testing.T) {
	s := masqueradeScript()
	chain := strings.Index(s, "type nat hook postrouting")
	flush := strings.Index(s, "flush chain ip "+NATTableName+" postrouting")
	rule := strings.Index(s, "masquerade")
	if chain < 0 || flush < 0 || rule < 0 {
		t.Fatalf("script missing chain, flush or rules:\n%s", s)
	}
	if !(chain < flush && flush < rule) {
		t.Fatalf("want chain declaration, then flush, then rules:\n%s", s)
	}
	if n := strings.Count(s, "masquerade"); n != 2 {
		t.Fatalf("script adds %d masquerade rules, want 2", n)
	}
}
