package registry

import (
	"strings"
	"testing"
)

func TestBroker_AuditBudgetOverflow(t *testing.T) {
	b := parseBroker(t, brokerWithProps(`    text:
      type: array
      maxItems: 1073741824
      items:
        type: array
        maxItems: 1073741824
        items: {type: string, maxLength: 512, x-untrusted: true}
`))
	if err := b.Validate(); err == nil {
		t.Fatal("overflowed untrusted input budget accepted")
	}
}

func TestBroker_AuditProposalPropertyNames(t *testing.T) {
	for _, name := range []string{"a.b", "notes[]", "nested[0]"} {
		b := parseBroker(t, validBrokerYAML+"proposes:"+validProposal)
		props := b.Proposes[0].ArgsSchema["properties"].(map[string]any)
		props[name] = map[string]any{"type": "string", "x-untrusted": true, "maxLength": 64}
		if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "dots or brackets") {
			t.Fatalf("proposal property %q: want unambiguous-name refusal, got %v", name, err)
		}
	}
}
