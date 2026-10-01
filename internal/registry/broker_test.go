package registry

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Broker workflow load rules — https://docs.vornik.io
// 2026-09-29-companion-privileged-work-broker-design.md §4.2. A broker
// workflow is the only thing a front agent's key may run, so these rules are
// the input half of the boundary: a string the loader lets through
// unconstrained is a free-text instruction channel from a steerable agent into
// the agent that holds the credentials.

const validBrokerYAML = `
input_schema:
  type: object
  additionalProperties: false
  required: [since]
  properties:
    since:      { type: string, format: date-time }
    folders:    { type: array, maxItems: 5, items: { enum: [INBOX, Work, Finance] } }
    importance: { enum: [all, high] }
    limit:      { type: integer, minimum: 1, maximum: 30 }
    topic:      { type: string, maxLength: 120, x-untrusted: true }
egress:
  output: digest.json
  max_bytes: 16384
  schema:
    type: object
    required: [items]
    properties:
      items:
        type: array
        maxItems: 30
        items:
          type: object
          properties:
            one_line: { type: string, maxLength: 200 }
`

func parseBroker(t *testing.T, src string) *WorkflowBroker {
	t.Helper()
	var b WorkflowBroker
	if err := yaml.Unmarshal([]byte(src), &b); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	return &b
}

// mutateInput replaces the input_schema properties block of the valid
// broker with props (a YAML mapping body indented for the properties key).
func brokerWithProps(props string) string {
	return `
input_schema:
  type: object
  additionalProperties: false
  properties:
` + props + `
egress:
  output: digest.json
  schema: { type: object }
`
}

func TestBrokerValidate_AcceptsReferenceShape(t *testing.T) {
	b := parseBroker(t, validBrokerYAML)
	if err := b.Validate(); err != nil {
		t.Fatalf("reference broker block must load: %v", err)
	}
	if got := b.Egress.EffectiveProvenance(); got != BrokerProvenanceThirdParty {
		t.Fatalf("provenance default = %q, want third_party", got)
	}
	if got := b.Egress.EffectiveMaxBytes(); got != 16384 {
		t.Fatalf("max_bytes = %d, want 16384", got)
	}
}

func TestBrokerValidate_Refusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"missing egress output", strings.Replace(validBrokerYAML, "  output: digest.json\n", "", 1), "egress.output"},
		{"missing egress schema", `
input_schema: { type: object, additionalProperties: false, properties: {} }
egress: { output: d.json }
`, "egress.schema"},
		{"input not object", `
input_schema: { type: string, enum: [a] }
egress: { output: d.json, schema: { type: object } }
`, "type: object"},
		{"additionalProperties open", `
input_schema: { type: object, properties: {} }
egress: { output: d.json, schema: { type: object } }
`, "additionalProperties"},
		{"unconstrained string", brokerWithProps("    notes: { type: string }"), "input_schema.properties.notes"},
		{"untrusted without maxLength", brokerWithProps("    q: { type: string, x-untrusted: true }"), "maxLength"},
		{"untrusted maxLength over 512", brokerWithProps("    q: { type: string, x-untrusted: true, maxLength: 513 }"), "512"},
		{"pattern without maxLength", brokerWithProps("    q: { type: string, pattern: '^[a-z]+$' }"), "maxLength"},
		{"unknown format", brokerWithProps("    q: { type: string, format: hostname-ish }"), "format"},
		{"array without maxItems", brokerWithProps("    xs: { type: array, items: { enum: [a] } }"), "maxItems"},
		{"nested object open", brokerWithProps("    o: { type: object, properties: { a: { enum: [x] } } }"), "additionalProperties"},
		{"oneOf refused", brokerWithProps("    o: { oneOf: [ { enum: [a] }, { enum: [b] } ] }"), "oneOf"},
		{"anyOf refused", brokerWithProps("    o: { anyOf: [ { enum: [a] } ] }"), "anyOf"},
		{"ref refused", brokerWithProps("    o: { $ref: '#/x' }"), "$ref"},
		{"patternProperties refused", `
input_schema: { type: object, additionalProperties: false, patternProperties: { "^x": { enum: [a] } } }
egress: { output: d.json, schema: { type: object } }
`, "patternProperties"},
		{"untrusted budget over 1024 flat", brokerWithProps(
			"    a: { type: string, x-untrusted: true, maxLength: 512 }\n" +
				"    b: { type: string, x-untrusted: true, maxLength: 512 }\n" +
				"    c: { type: string, x-untrusted: true, maxLength: 1 }"), "1024"},
		// Review F1 (review-20260929-e1a1): the budget multiplies by EVERY
		// array ancestor. Nearest-ancestor-only would read 100 × 10 = 1000 and
		// pass; the real capacity is 10 × 10 × 100 = 10 000.
		{"untrusted budget nested arrays", brokerWithProps(
			"    xs:\n      type: array\n      maxItems: 10\n      items:\n        type: array\n        maxItems: 10\n        items: { type: string, x-untrusted: true, maxLength: 100 }"), "1024"},
		{"bad provenance", strings.Replace(validBrokerYAML, "  max_bytes: 16384\n", "  max_bytes: 16384\n  provenance: mostly_fine\n", 1), "provenance"},
		{"max_bytes over ceiling", strings.Replace(validBrokerYAML, "max_bytes: 16384", "max_bytes: 70000", 1), "max_bytes"},
		{"egress output not json", strings.Replace(validBrokerYAML, "output: digest.json", "output: digest.md", 1), ".json"},
		{"egress output with path", strings.Replace(validBrokerYAML, "output: digest.json", "output: ../digest.json", 1), "egress.output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseBroker(t, tc.src).Validate()
			if err == nil {
				t.Fatalf("expected refusal mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestBrokerValidate_UntrustedBudgetExactlyAtLimit(t *testing.T) {
	src := brokerWithProps(
		"    a: { type: string, x-untrusted: true, maxLength: 512 }\n" +
			"    b: { type: string, x-untrusted: true, maxLength: 512 }")
	if err := parseBroker(t, src).Validate(); err != nil {
		t.Fatalf("1024 exactly must load: %v", err)
	}
}

func TestBrokerUntrustedPaths(t *testing.T) {
	b := parseBroker(t, validBrokerYAML)
	got := b.UntrustedInputPaths()
	if len(got) != 1 || got[0] != "topic" {
		t.Fatalf("untrusted paths = %v, want [topic]", got)
	}
}

func TestWorkflowValidate_RejectsInvalidBrokerBlock(t *testing.T) {
	wf := &Workflow{
		ID:         "mail-digest",
		Entrypoint: "read",
		Steps: map[string]WorkflowStep{
			"read": {Type: "agent", Role: "reader", OnSuccess: "done"},
		},
		Terminals: map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}},
		Broker:    parseBroker(t, brokerWithProps("    notes: { type: string }")),
	}
	err := wf.Validate("mail-digest.md")
	if err == nil || !strings.Contains(err.Error(), "broker") {
		t.Fatalf("Validate must refuse an invalid broker block, got %v", err)
	}
	wf.Broker = parseBroker(t, validBrokerYAML)
	if err := wf.Validate("mail-digest.md"); err != nil {
		t.Fatalf("valid broker workflow must load: %v", err)
	}
}

// proposes — broker write-actions design (2026-09-29) §4.1.
const validProposal = `
  - action: gmail_reply
    tool: mcp__google-workspace__gmail_send
    output: proposal.json
    args_schema:
      type: object
      additionalProperties: false
      required: [to, body]
      properties:
        to:   { type: string, format: email, maxLength: 254 }
        body: { type: string, maxLength: 4000, x-untrusted: true }
`

func TestBrokerProposes_AcceptsDeclaredWrite(t *testing.T) {
	b := parseBroker(t, validBrokerYAML+"proposes:"+validProposal)
	if err := b.Validate(); err != nil {
		t.Fatalf("a valid proposal must load: %v", err)
	}
	p := b.Proposes[0]
	if p.EffectiveMaxArgsBytes() != 8192 {
		t.Fatalf("max_args_bytes default = %d", p.EffectiveMaxArgsBytes())
	}
	if ttl, _ := p.EffectiveApprovalTTL(); ttl.Hours() != 24 {
		t.Fatalf("approval_ttl default = %v", ttl)
	}
	if server, tool, ok := p.ServerTool(); !ok || server != "google-workspace" || tool != "gmail_send" {
		t.Fatalf("ServerTool = %q %q %v", server, tool, ok)
	}
}

func TestBrokerProposes_Refusals(t *testing.T) {
	cases := []struct{ name, proposes, want string }{
		{"duplicate action", validProposal + strings.Replace(validProposal, "proposal.json", "p2.json", 1), "duplicate"},
		{"output collides with egress", strings.Replace(validProposal, "proposal.json", "digest.json", 1), "egress.output"},
		{"output not json", strings.Replace(validProposal, "proposal.json", "proposal.md", 1), ".json"},
		{"output is a path", strings.Replace(validProposal, "proposal.json", "../p.json", 1), "bare file name"},
		{"tool not mcp", strings.Replace(validProposal, "mcp__google-workspace__gmail_send", "send_email", 1), "mcp__"},
		{"tool wildcard", strings.Replace(validProposal, "gmail_send", "gmail_*", 1), "mcp__"},
		{"bad action name", strings.Replace(validProposal, "gmail_reply", "Gmail Reply!", 1), "action"},
		{"unbounded arg", strings.Replace(validProposal, "format: email, maxLength: 254", "maxLength: 254", 1), "unconstrained"},
		{"max_args_bytes over ceiling", strings.Replace(validProposal, "    output: proposal.json\n", "    output: proposal.json\n    max_args_bytes: 40000\n", 1), "max_args_bytes"},
		{"ttl too short", strings.Replace(validProposal, "    output: proposal.json\n", "    output: proposal.json\n    approval_ttl: 1m\n", 1), "approval_ttl"},
		{"ttl too long", strings.Replace(validProposal, "    output: proposal.json\n", "    output: proposal.json\n    approval_ttl: 200h\n", 1), "approval_ttl"},
		{"missing args_schema", "\n  - action: a\n    tool: mcp__s__t\n    output: p.json\n", "args_schema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseBroker(t, validBrokerYAML+"proposes:"+tc.proposes).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want refusal mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

// format: email is bounded enough for a recipient, but only in args_schema:
// an input field is written by the front agent, and an address there is a
// free channel.
func TestBrokerInputSchema_StillRefusesEmailFormat(t *testing.T) {
	err := parseBroker(t, brokerWithProps("    who: { type: string, format: email }")).Validate()
	if err == nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("email format must stay refused in input_schema, got %v", err)
	}
}

// review-20260930-32ae: an open args_schema object is refused, like an open
// input_schema object (the walker is shared).
func TestBrokerProposes_OpenArgsObjectRefused(t *testing.T) {
	open := strings.Replace(validProposal, "      additionalProperties: false\n", "", 1)
	err := parseBroker(t, validBrokerYAML+"proposes:"+open).Validate()
	if err == nil || !strings.Contains(err.Error(), "additionalProperties") {
		t.Fatalf("an open args_schema must be refused, got %v", err)
	}
}

func TestCheckBrokerProposals_DisabledIsASentinel(t *testing.T) {
	p, wf, _ := proposingFixture()
	if err := CheckBrokerProposals(p, wf, false); !errors.Is(err, ErrBrokerWritesDisabled) {
		t.Fatalf("want ErrBrokerWritesDisabled, got %v", err)
	}
}

// The /inbox card flags the fields a model drafted from third-party content
// (write-actions design §5.3).
func TestBrokerProposal_UntrustedArgPaths(t *testing.T) {
	b := parseBroker(t, validBrokerYAML+"proposes:"+validProposal)
	if got := b.Proposes[0].UntrustedArgPaths(); strings.Join(got, ",") != "body" {
		t.Fatalf("UntrustedArgPaths = %v, want [body]", got)
	}
	if got := (BrokerProposal{}).UntrustedArgPaths(); got != nil {
		t.Fatalf("no schema: %v", got)
	}
}
