package imageref

import "testing"

func TestCanonicalIsTheOneMapping(t *testing.T) {
	if got := Canonical("ghcr.io/grinco/vornik-agent:latest"); got != AgentRepo+":latest" {
		t.Fatalf("got %q", got)
	}
	if AgentRegistry+"vornik-agent" != AgentRepo {
		t.Fatal("AgentRegistry must be AgentRepo's registry path")
	}
}
