package api

import (
	"strings"
	"testing"
)

// audit 2026-10-12 (finding G): the status tool's description listed the
// broker error classes without egress_secret, which an agent workflow's
// egress returns when its document carries a credential-shaped value. Every
// class status can return is named in the description a client reads.
func TestCompanionStatusDescription_NamesEveryBrokerErrorClass(t *testing.T) {
	var desc string
	for _, d := range companionToolDefs() {
		if d.Name == "status" {
			desc = d.Description
		}
	}
	if desc == "" {
		t.Fatal("no status tool")
	}
	classes := []string{brokerErrFailed, brokerErrTimeout, brokerErrBudget, brokerErrEgressNoOutput,
		brokerErrEgressOversize, brokerErrEgressSchema, brokerErrEgressSecret}
	for _, c := range classes {
		if !strings.Contains(desc, c) {
			t.Errorf("status description does not name error class %q", c)
		}
	}
	t.Logf("examined %d error classes", len(classes))
}
