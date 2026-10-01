package registry

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Broker write-actions design §7a: companion_push.allowed_cidrs lists the
// private ranges a project's pushes may reach. Loopback, link-local, public
// and malformed entries fail the load.
func TestProjectCompanionPushAllowedCIDRs(t *testing.T) {
	base := `
projectId: p
swarmId: s
defaultWorkflowId: w
defaultPriority: 50
`
	load := func(extra string) error {
		var p Project
		if err := yaml.Unmarshal([]byte(base+extra), &p); err != nil {
			return err
		}
		return p.Validate("p.yaml")
	}
	if err := load("companion_push:\n  allowed_cidrs: [\"192.168.1.0/24\", \"fd00::/8\", \"100.64.0.0/10\"]\n"); err != nil {
		t.Fatalf("valid ranges refused: %v", err)
	}
	if err := load(""); err != nil {
		t.Fatalf("absent block refused: %v", err)
	}
	for _, bad := range []string{"127.0.0.0/8", "169.254.0.0/16", "8.8.8.0/24", "garbage"} {
		err := load("companion_push:\n  allowed_cidrs: [\"" + bad + "\"]\n")
		if err == nil || !strings.Contains(err.Error(), "companion_push.allowed_cidrs") {
			t.Errorf("%q: want a load error naming companion_push.allowed_cidrs, got %v", bad, err)
		}
	}
	var p Project
	if err := yaml.Unmarshal([]byte(base+"companion_push:\n  allowed_cidrs: [\"10.0.0.0/8\"]\n"), &p); err != nil {
		t.Fatal(err)
	}
	if got := p.CompanionPushAllowed(); len(got) != 1 || got[0].String() != "10.0.0.0/8" {
		t.Fatalf("CompanionPushAllowed = %v", got)
	}
	if got := (&Project{}).CompanionPushAllowed(); got != nil {
		t.Fatalf("no block: %v", got)
	}
}
