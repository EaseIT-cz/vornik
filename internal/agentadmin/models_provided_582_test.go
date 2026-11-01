package agentadmin

// GitHub #582 P3 (operations-api-contract-design.md §6.2 P3): in managed
// hosting a model the gateway serves (its host in hosting.model_hosts, which
// the resolver marks Provided) is its own class: not local, and not a remote
// destination needing an approver-device approval or a pricing.yaml entry.
// Found at: design review (6.1 B3).

import (
	"strings"
	"testing"
)

func TestProvided582_Classification(t *testing.T) {
	gw := ModelRoute{SubProvider: "gateway", Endpoint: "https://gateway.internal/v1", Provided: true}
	same := ModelRoute{SubProvider: "gateway", Endpoint: "https://gateway.internal/v1"} // self hosting: not provided
	resolve := func(r ModelRoute) ModelResolver { return func(string) (ModelRoute, bool) { return r, true } }
	unpriced := func(string) (float64, float64, bool) { return 0, 0, false }
	specs := []ModelSpec{{ID: "open-a", GoodFor: "general"}}

	d, why := DestinationOf(gw, true)
	if why != "" || !d.Provided || d.Local {
		t.Fatalf("a provided route: %+v %q, want provided and not local", d, why)
	}

	// Offered without a price, and described as provided.
	cat, findings := BuildCatalogue(specs, resolve(gw), unpriced)
	m, ok := cat["open-a"]
	if !ok || len(findings) != 0 {
		t.Fatalf("an unpriced provided model must be offered: %v %v", cat, findings)
	}
	if !strings.Contains(m.Where(), "provided") || !strings.Contains(m.PriceLabel(), "metered by the hosting service") {
		t.Errorf("Where %q, PriceLabel %q", m.Where(), m.PriceLabel())
	}
	// The same host without the mark is remote: unpriced, so dropped.
	if cat, _ := BuildCatalogue(specs, resolve(same), unpriced); len(cat) != 0 {
		t.Error("without the provided mark an unpriced remote model must not be offered")
	}

	// It runs with no approval.
	never := func(string) bool { return false }
	if _, refusal := RunModelRefusal("open-a", specs, resolve(gw), never); refusal.Reason != "" {
		t.Errorf("a provided model needs no approval: %+v", refusal)
	}
	if _, refusal := RunModelRefusal("open-a", specs, resolve(same), never); refusal.Reason != RefusalUnapproved {
		t.Errorf("the unmarked route must still need approval: %+v", refusal)
	}
	if got := RoleRunsOn("open-a", specs, resolve(gw), never); got != "provided" {
		t.Errorf("RoleRunsOn = %q, want provided", got)
	}
}
