package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

const mailArgs = `{"type":"object","additionalProperties":false,"required":["to"],"properties":{"to":{"type":"string","format":"email"},"subject":{"type":"string","maxLength":200,"x-untrusted":true}}}`

func proposingTree(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t, "hermes")
	tr.project("comms")
	tr.advert["https://mail.example/mcp"] = []string{"read_inbox", "send", "delete"}
	tr.apply(tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "mail", URL: "https://mail.example/mcp", WriteTools: []string{"send"}}))
	tr.apply(tr.render(VerbAddAPI, AddAPIInput{Project: "comms", Name: "pay", BaseURL: "https://pay.example", Methods: []string{"GET", "POST"}, Writes: true}))
	return tr
}

func proposeWF(tr *tree, proposes string) Change {
	tr.t.Helper()
	return tr.render(VerbDefineWorkflow, DefineWorkflowInput{Project: "comms", Slug: "reply", Purpose: "reply to mail",
		Steps:    []StepInput{{Name: "draft", Role: "worker", Instructions: "Draft a reply."}},
		Egress:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"done":{"type":"boolean"}}}`),
		Proposes: json.RawMessage(proposes)})
}

// Plan P4.8: a workflow may propose writes of its project's approved write
// entries and API write methods; each proposal is rendered with its own
// output and is part of the workflow's reach (widening). Control: the
// proposes branch of defineWorkflow.
func TestDefineWorkflow_Proposes(t *testing.T) {
	tr := proposingTree(t)
	c := proposeWF(tr, `[{"action":"send_mail","tool":"mcp__mail-write__send","args_schema":`+mailArgs+`},
		{"action":"pay","tool":"api:pay:POST:/payments","args_schema":{"type":"object","additionalProperties":false,"properties":{"amount":{"type":"number"}}}}]`)
	tr.mustClass(c, Widening)
	md := c.Ops[0].Content
	for _, want := range []string{`tool: "mcp__mail-write__send"`, `output: "propose-send_mail.json"`, `tool: "api:pay:POST:/payments"`, "propose-pay.json"} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered workflow lacks %q:\n%s", want, md)
		}
	}
	// The prompt gives the proposal file's exact shape, the one staging
	// parses: {"action": ..., "args": {...}} (found in P5: it said only
	// "its arguments", so every proposal would have staged invalid).
	if !strings.Contains(md, `{"action": "send_mail", "args": {`) {
		t.Errorf("the prompt does not give the proposal shape:\n%s", md)
	}
	for _, want := range []string{"send_mail", "pay", "approve each"} {
		if !strings.Contains(c.Sentence, want) {
			t.Errorf("sentence lacks %q: %s", want, c.Sentence)
		}
	}
	for name, proposes := range map[string]string{
		"write tool not approved":  `[{"action":"wipe","tool":"mcp__mail-write__delete","args_schema":` + mailArgs + `}]`,
		"write via the read entry": `[{"action":"send","tool":"mcp__mail__send","args_schema":` + mailArgs + `}]`,
		"unknown server":           `[{"action":"send","tool":"mcp__chat-write__send","args_schema":` + mailArgs + `}]`,
		"API method not approved":  `[{"action":"del","tool":"api:pay:DELETE:/x","args_schema":` + mailArgs + `}]`,
		"unknown API":              `[{"action":"p","tool":"api:bank:POST:/x","args_schema":` + mailArgs + `}]`,
		"duplicate action":         `[{"action":"a","tool":"mcp__mail-write__send","args_schema":` + mailArgs + `},{"action":"a","tool":"mcp__mail-write__send","args_schema":` + mailArgs + `}]`,
		"bad action name":          `[{"action":"Send Mail","tool":"mcp__mail-write__send","args_schema":` + mailArgs + `}]`,
		"unconstrained args":       `[{"action":"s","tool":"mcp__mail-write__send","args_schema":{"type":"object","additionalProperties":false,"properties":{"to":{"type":"string"}}}}]`,
		"not an array":             `{"action":"s"}`,
	} {
		if c := proposeWF(tr, proposes); c.Class != Refused {
			t.Errorf("%s: accepted", name)
		}
	}
	tr.writesOff = true
	if c := proposeWF(tr, `[{"action":"send_mail","tool":"mcp__mail-write__send","args_schema":`+mailArgs+`}]`); c.Class != Refused || !strings.Contains(c.Reason, "writes are off") {
		t.Fatalf("proposals with broker.writes off: %v %s", c.Class, c.Reason)
	}
}

// Found while building P4.8: the renderer accepted an input schema the
// loader's broker rules refuse (an unconstrained string), so the change was
// filed, approved, and then failed to apply. The renderer now runs the
// loader's own broker validation. Control: the wf.Broker.Validate call.
func TestDefineWorkflow_RefusesWhatTheLoaderRefuses(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	c := tr.render(VerbDefineWorkflow, DefineWorkflowInput{Project: "finance", Slug: "spend",
		Steps:  []StepInput{{Name: "read", Role: "worker", Instructions: "x"}},
		Inputs: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"month":{"type":"string","maxLength":7}}}`),
		Egress: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"n":{"type":"number"}}}`)})
	if c.Class != Refused || !strings.Contains(c.Reason, "unconstrained string") {
		t.Fatalf("a schema the loader refuses: %v %s", c.Class, c.Reason)
	}
}
