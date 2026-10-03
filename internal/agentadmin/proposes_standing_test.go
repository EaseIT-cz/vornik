package agentadmin

import (
	"strings"
	"testing"
)

// Standing grants through define_workflow — broker write-actions design,
// "Tier 2, revised" item 1: a proposal may declare standing; it is rendered
// into the workflow, part of the reach (widening), stated in the approval
// sentence with the fields a covered write sends unseen, and refused when
// the loader would refuse it.

const standingArgs = `{"type":"object","additionalProperties":false,"required":["to"],"properties":{"to":{"type":"string","format":"email","maxLength":254,"x-destination":true},"subject":{"type":"string","maxLength":200,"x-untrusted":true}}}`

func TestDefineWorkflow_ProposesWithStanding(t *testing.T) {
	tr := proposingTree(t)
	c := proposeWF(tr, `[{"action":"send_mail","tool":"mcp__mail-write__send","args_schema":`+standingArgs+`,"standing":{"key":["to"],"max_days":7,"max_uses":10}}]`)
	tr.mustClass(c, Widening)
	md := c.Ops[0].Content
	if !strings.Contains(md, `standing: {"key":["to"],"max_days":7,"max_uses":10}`) {
		t.Fatalf("the rendered workflow lacks the standing declaration:\n%s", md)
	}
	for _, want := range []string{"without showing you their text", "subject"} {
		if !strings.Contains(c.Sentence, want) {
			t.Errorf("sentence lacks %q: %s", want, c.Sentence)
		}
	}
	for name, proposes := range map[string]string{
		"no destination marker": `[{"action":"s","tool":"mcp__mail-write__send","args_schema":` + mailArgs + `,"standing":{"key":["to"]}}]`,
		"key misses a destination": `[{"action":"s","tool":"mcp__mail-write__send","args_schema":` +
			strings.Replace(standingArgs, `"subject":{`, `"cc":{"type":"string","format":"email","maxLength":254,"x-destination":true},"subject":{`, 1) +
			`,"standing":{"key":["to"]}}]`,
		"uses over 20":  `[{"action":"s","tool":"mcp__mail-write__send","args_schema":` + standingArgs + `,"standing":{"key":["to"],"max_uses":50}}]`,
		"unknown field": `[{"action":"s","tool":"mcp__mail-write__send","args_schema":` + standingArgs + `,"standing":{"key":["to"],"forever":true}}]`,
	} {
		if c := proposeWF(tr, proposes); c.Class != Refused {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Declaring standing on an approved workflow is a reach change: widening,
// back to the device (5c20 F6).
func TestDefineWorkflow_AddingStandingIsWidening(t *testing.T) {
	tr := proposingTree(t)
	plain := proposeWF(tr, `[{"action":"send_mail","tool":"mcp__mail-write__send","args_schema":`+standingArgs+`}]`)
	tr.mustClass(plain, Widening)
	tr.apply(plain)
	if again := proposeWF(tr, `[{"action":"send_mail","tool":"mcp__mail-write__send","args_schema":`+standingArgs+`}]`); again.Class == Widening {
		t.Fatalf("an unchanged workflow is widening again: %v", again.Class)
	}
	with := proposeWF(tr, `[{"action":"send_mail","tool":"mcp__mail-write__send","args_schema":`+standingArgs+`,"standing":{"key":["to"]}}]`)
	tr.mustClass(with, Widening)
}
