package registry

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Standing grants — broker write-actions design, "Tier 2, revised" item 1
// (eligibility) and item 9 (bounds), as settled by review 61a5.

const standingProposal = `
  - action: send_reply
    tool: mcp__mail-write__gmail_send
    output: proposal.json
    standing: { key: [to, cc, bcc], max_days: 7, max_uses: 20 }
    args_schema:
      type: object
      additionalProperties: false
      required: [to, body]
      properties:
        to:   { type: string, format: email, maxLength: 254, x-destination: true }
        cc:   { type: array, maxItems: 5, items: { type: string, format: email, maxLength: 254 }, x-destination: true }
        bcc:  { type: array, maxItems: 5, items: { type: string, format: email, maxLength: 254 }, x-destination: true }
        body: { type: string, maxLength: 4000, x-untrusted: true }
`

func TestBrokerStanding_AcceptsAKeyCoveringEveryDestination(t *testing.T) {
	b := parseBroker(t, validBrokerYAML+"proposes:"+standingProposal)
	if err := b.Validate(); err != nil {
		t.Fatalf("a standing declaration over every destination must load: %v", err)
	}
	p := b.Proposes[0]
	if p.Standing == nil || !reflect.DeepEqual(p.Standing.Key, []string{"to", "cc", "bcc"}) {
		t.Fatalf("standing = %+v", p.Standing)
	}
	if got := p.DestinationArgPaths(); !reflect.DeepEqual(got, []string{"bcc", "cc", "to"}) {
		t.Fatalf("DestinationArgPaths = %v", got)
	}
	if p.Standing.EffectiveMaxDays() != 7 || p.Standing.EffectiveMaxUses() != 20 {
		t.Fatal("bounds not read")
	}
}

// Defaults: an omitted max_days / max_uses is the ceiling (7 days, 20 uses).
func TestBrokerStanding_DefaultsAreTheCeilings(t *testing.T) {
	src := strings.Replace(standingProposal, ", max_days: 7, max_uses: 20", "", 1)
	b := parseBroker(t, validBrokerYAML+"proposes:"+src)
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	if s := b.Proposes[0].Standing; s.EffectiveMaxDays() != 7 || s.EffectiveMaxUses() != 20 {
		t.Fatalf("defaults = %d days, %d uses", s.EffectiveMaxDays(), s.EffectiveMaxUses())
	}
}

// Tests (failing first) of "Tier 2, revised": no destination marker, an
// attachment-carrying schema, a key missing cc: refused at load; bounds above
// their limits refused.
func TestBrokerStanding_Refusals(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"key missing cc", strings.Replace(standingProposal, "key: [to, cc, bcc]", "key: [to, bcc]", 1), "cc"},
		{"no destination marker", strings.ReplaceAll(standingProposal, ", x-destination: true", ""), "x-destination"},
		{"carries content", strings.Replace(standingProposal, "        body:", "        attachment: { type: string, maxLength: 200, pattern: \"^[a-z]+$\", x-carries-content: true }\n        body:", 1), "x-carries-content"},
		{"empty key", strings.Replace(standingProposal, "key: [to, cc, bcc]", "key: []", 1), "key"},
		{"unknown key path", strings.Replace(standingProposal, "key: [to, cc, bcc]", "key: [to, cc, bcc, nope]", 1), "nope"},
		{"duplicate key path", strings.Replace(standingProposal, "key: [to, cc, bcc]", "key: [to, cc, bcc, to]", 1), "twice"},
		{"days over 7", strings.Replace(standingProposal, "max_days: 7", "max_days: 8", 1), "max_days"},
		{"uses over 20", strings.Replace(standingProposal, "max_uses: 20", "max_uses: 21", 1), "max_uses"},
		{"negative days", strings.Replace(standingProposal, "max_days: 7", "max_days: -1", 1), "max_days"},
		{"nested destination", strings.Replace(standingProposal, "        body:", "        meta: { type: object, additionalProperties: false, properties: { reply_to: { type: string, format: email, maxLength: 254, x-destination: true } } }\n        body:", 1), "top-level"},
		{"destination on a body-like field", strings.Replace(standingProposal, "x-untrusted: true }", "x-untrusted: true, x-destination: true }", 1), "body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseBroker(t, validBrokerYAML+"proposes:"+tc.src).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

// x-carries-content makes a schema ineligible for standing, and only that:
// a proposal without standing may still carry content.
func TestBrokerStanding_CarriesContentWithoutStandingLoads(t *testing.T) {
	src := strings.Replace(validProposal, "        body:", "        attachment: { type: string, maxLength: 200, pattern: \"^[a-z]+$\", x-carries-content: true }\n        body:", 1)
	if err := parseBroker(t, validBrokerYAML+"proposes:"+src).Validate(); err != nil {
		t.Fatalf("x-carries-content without standing must load: %v", err)
	}
}

// "Tier 2, revised" item 1 and review 5c20 F6: an absent standing
// contributes nothing to the proposal's JSON, the reach signature's input.
func TestBrokerStanding_AbsentStandingIsAbsentFromJSON(t *testing.T) {
	b := parseBroker(t, validBrokerYAML+"proposes:"+validProposal)
	raw, err := json.Marshal(b.Proposes[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "standing") {
		t.Fatalf("a proposal without standing marshals %s", raw)
	}
}
