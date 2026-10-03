package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Broker design §18.4 and §18.7 F1/F3 (a document input, GREEN at review
// 7514), and agent-administered design §7.6 as amended with it: a document
// bound is reach. The signature gains Documents; a workflow with none hashes
// exactly as before; the approval sentence and the plain summary name the
// document, its cap and its type.

// reachHashWithInputs is reachHashOf with the fixture's input schema
// replaced.
func reachHashWithInputs(t *testing.T, inputSchema string) string {
	t.Helper()
	md := strings.Replace(reachFixture, "STANDING\n", "", 1)
	md = strings.Replace(md, `input_schema: {"type":"object","additionalProperties":false,"properties":{}}`, "input_schema: "+inputSchema, 1)
	wf, err := registry.ParseWorkflowMarkdown([]byte(md), "ns1--mail.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := wf.Broker.Validate(); err != nil {
		t.Fatal(err)
	}
	sig, err := SignatureOf(nil, nil, wf)
	if err != nil {
		t.Fatal(err)
	}
	return sig.Hash()
}

func docSchema(maxBytes int, mediaType string) string {
	b, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"design": map[string]any{"type": "string", "x-untrusted-document": map[string]any{"max_bytes": maxBytes, "media_type": mediaType}},
	}})
	return string(b)
}

// §18.7 F1: the golden hash (computed on main before documents existed)
// still verifies for a workflow without one, so nothing approved today is
// re-approved; and every other input stays outside the signature.
func TestReach_NoDocumentKeepsTheHash(t *testing.T) {
	if got := reachHashOf(t, ""); got != reachGoldenNoStanding {
		t.Fatalf("reach hash of a workflow without a document = %s, want the pre-document %s", got, reachGoldenNoStanding)
	}
	strings1 := `{"type":"object","additionalProperties":false,"properties":{"topic":{"type":"string","x-untrusted":true,"maxLength":200},"n":{"type":"integer"}}}`
	if got := reachHashWithInputs(t, strings1); got != reachGoldenNoStanding {
		t.Fatal("a string or number input changed the reach hash; only documents are reach")
	}
}

// §18.4: declaring a document, raising its bound or changing its type is a
// reach change.
func TestReach_DocumentIsReach(t *testing.T) {
	with := reachHashWithInputs(t, docSchema(65536, "text/markdown"))
	if with == reachGoldenNoStanding {
		t.Fatal("declaring a document did not change the reach hash")
	}
	if reachHashWithInputs(t, docSchema(131072, "text/markdown")) == with {
		t.Fatal("raising max_bytes did not change the reach hash")
	}
	if reachHashWithInputs(t, docSchema(65536, "text/plain")) == with {
		t.Fatal("changing media_type did not change the reach hash")
	}
}

func defineDocWF(tr *tree, role string, inputs string) Change {
	tr.t.Helper()
	return tr.render(VerbDefineWorkflow, DefineWorkflowInput{Project: "finance", Slug: "review", Purpose: "review a design",
		Steps:  []StepInput{{Name: "read", Role: role, Instructions: "Review the design."}},
		Inputs: json.RawMessage(inputs),
		Egress: egress1()})
}

// §18.4 and §18.7 F3: the sentence names the bound and type and, with no
// connection, says nothing leaves Vornik; adding a document to an approved
// workflow, or raising its bound, is widening.
func TestDefineWorkflow_DocumentSentenceAndWidening(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	plain := defineDocWF(tr, "worker", `{"type":"object","additionalProperties":false,"properties":{}}`)
	tr.mustClass(plain, Widening)
	if strings.Contains(plain.Sentence, "document") {
		t.Fatalf("a workflow without a document mentions one: %q", plain.Sentence)
	}
	tr.apply(plain)

	added := defineDocWF(tr, "worker", docSchema(65536, "text/markdown"))
	tr.mustClass(added, Widening)
	want := "It takes from your assistant a document of up to 64 KB (text/markdown); nothing leaves Vornik."
	if !strings.Contains(added.Sentence, want) {
		t.Fatalf("sentence %q lacks %q", added.Sentence, want)
	}
	if added.Plain == nil || !strings.Contains(added.Plain.Summary, "a document of up to 64 KB (text/markdown)") {
		t.Fatalf("plain summary does not name the document: %+v", added.Plain)
	}
	tr.apply(added)

	tr.mustClass(defineDocWF(tr, "worker", docSchema(65536, "text/markdown")), Inert)
	tr.mustClass(defineDocWF(tr, "worker", docSchema(131072, "text/markdown")), Widening)
}

// §18.7 F3: with a connection, the sentence says the team may use the text in
// requests to it.
func TestDefineWorkflow_DocumentSentenceNamesTheConnections(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	tr.approveServer("finance", "fio", "https://api.fio.example/mcp", "list_transactions", "balance")
	tr.apply(tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{
		{Name: "reader", Instructions: "Read.", Tools: []string{"file_write", "mcp__fio__balance"}}}}))
	c := defineDocWF(tr, "reader", docSchema(1000, "text/plain"))
	tr.mustClass(c, Widening)
	want := `It takes from your assistant a document of up to 1000 bytes (text/plain); the team may use its text in requests to "fio".`
	if !strings.Contains(c.Sentence, want) {
		t.Fatalf("sentence %q lacks %q", c.Sentence, want)
	}
	two := defineDocWF(tr, "reader", `{"type":"object","additionalProperties":false,"properties":{
		"a":{"type":"string","x-untrusted-document":{"max_bytes":2048,"media_type":"text/x-diff"}},
		"b":{"type":"string","x-untrusted-document":{"max_bytes":4096,"media_type":"text/markdown"}}}}`)
	tr.mustClass(two, Widening)
	if !strings.Contains(two.Sentence, "It takes from your assistant two documents, of up to 2 KB (text/x-diff) and 4 KB (text/markdown); the team may use their text in requests to") {
		t.Fatalf("two-document sentence: %q", two.Sentence)
	}
}

// §18.6: a refused document declaration is refused at define_workflow, with
// the document rule, before anything is filed.
func TestDefineWorkflow_DocumentRefusedShapes(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	c := defineDocWF(tr, "worker", docSchema(300000, "text/markdown"))
	if c.Class != Refused || !strings.Contains(c.Reason, "x-untrusted-document") {
		t.Fatalf("an over-cap document was not refused with the rule: %+v", c)
	}
}
