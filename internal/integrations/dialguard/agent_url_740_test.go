package dialguard

import (
	"slices"
	"testing"
)

// GitHub #740 (agent-administered design part 06, §6.8): the grant of
// agent_admin.private_api_hosts reaches agent MCP servers too, and whether it
// admits a URL is one predicate, PrivateHostGranted, which the guard and the
// pre-approval listing's mask both read.

func TestPrivateHostGranted_740(t *testing.T) {
	hosts := names("intranet.example", "10.0.0.5", "*.corp.example")
	for _, c := range []struct {
		url  string
		want bool
	}{
		{"https://intranet.example/mcp", true},
		{"https://Intranet.Example./mcp", true},
		{"http://intranet.example/mcp", false}, // https only
		{"https://other.example/mcp", false},   // not listed
		{"https://10.0.0.5/mcp", true},         // an IP-literal entry grants since #403 (§6.12)
		{"https://a.corp.example/mcp", false},  // a wildcard entry is not valid
		{"://intranet.example", false},         // unparsable
		{"https:///mcp", false},                // no host
	} {
		if got := PrivateHostGranted(c.url, hosts); got != c.want {
			t.Errorf("PrivateHostGranted(%q) = %v, want %v", c.url, got, c.want)
		}
	}
	if PrivateHostGranted("https://intranet.example/mcp", nil) {
		t.Error("an empty list granted")
	}
}

func TestPrivateHostGranted_740_ManagedPolicyGrantsNothing(t *testing.T) {
	withPolicy(t, &ManagedPolicy{})
	if PrivateHostGranted("https://intranet.example/mcp", names("intranet.example")) {
		t.Fatal("a managed policy granted a private host")
	}
}

// The guard adds PrivateHosts exactly when the predicate is true.
func TestAgentURLGuard_740_FollowsThePredicate(t *testing.T) {
	hosts := names("intranet.example")
	for _, u := range []string{"https://intranet.example/mcp", "http://intranet.example/mcp", "https://other.example/", "https://10.0.0.5/"} {
		g := AgentURLGuard(u, hosts)
		if granted := len(g.PrivateHosts) > 0; granted != PrivateHostGranted(u, hosts) {
			t.Errorf("%s: guard PrivateHosts=%v, predicate %v", u, g.PrivateHosts, PrivateHostGranted(u, hosts))
		}
	}
	if g := AgentURLGuard("https://Intranet.Example./mcp", hosts); !slices.Contains(g.PrivateHosts, "intranet.example") {
		t.Errorf("the folded spelling is not granted: %v", g.PrivateHosts)
	}
}
