package registry

import (
	"strings"
	"testing"
)

// Broker design §18 (a document input, GREEN at review 7514): a broker
// workflow may declare up to two top-level x-untrusted-document strings, each
// a bounded text document. These tests pin the load rules of §18.1 and §18.7
// (F2 the published rule, F6 the property name, F7 no application/json, F9
// refusals never echo the declaration's values).

func docProp(maxBytes any, mediaType string) map[string]any {
	return map[string]any{"type": "string", "description": "the text to review",
		"x-untrusted-document": map[string]any{"max_bytes": maxBytes, "media_type": mediaType}}
}

// §18.1: a valid declaration loads, and Documents lists it.
func TestBrokerDocument_ValidDeclarationLoads(t *testing.T) {
	b := inputBroker(obj(map[string]any{
		"design": docProp(float64(65536), "text/markdown"),
		"diff":   docProp(float64(262144), "text/x-diff"),
		"topic":  map[string]any{"type": "string", "x-untrusted": true, "maxLength": float64(512)},
	}))
	if err := b.Validate(); err != nil {
		t.Fatalf("a valid document declaration was refused: %v", err)
	}
	docs := b.Documents()
	if len(docs) != 2 || docs[0].Property != "design" || docs[1].Property != "diff" {
		t.Fatalf("Documents() = %+v", docs)
	}
	if docs[0].MaxBytes != 65536 || docs[0].MediaType != "text/markdown" || docs[0].Path() != "artifacts/in/design.md" {
		t.Fatalf("design = %+v, path %s", docs[0], docs[0].Path())
	}
	if docs[1].Path() != "artifacts/in/diff.diff" {
		t.Fatalf("diff path = %s", docs[1].Path())
	}
	// The document is not an x-untrusted string: it is never inlined, so it
	// is not wrapped either (§18.3), and it is not in the 1,024 budget.
	if got := b.UntrustedInputPaths(); len(got) != 1 || got[0] != "topic" {
		t.Fatalf("UntrustedInputPaths = %v", got)
	}
	if (BrokerDocument{Property: "a", MediaType: "text/plain"}).Path() != "artifacts/in/a.txt" {
		t.Fatal("text/plain is not staged as .txt")
	}
}

// §18.6 registry refusals, plus §18.7 F6 (the name) and F9 (no value in the
// message): each refused shape names the document rule.
func TestBrokerDocument_RefusedShapes(t *testing.T) {
	const canaryType = "application/x-canary-7514"
	withMaxLen := docProp(float64(1024), "text/plain")
	withMaxLen["maxLength"] = float64(1024)
	cases := map[string]struct {
		schema map[string]any
		canary string
	}{
		"over the byte cap":                   {obj(map[string]any{"d": docProp(float64(262145), "text/plain")}), "262145"},
		"zero bytes":                          {obj(map[string]any{"d": docProp(float64(0), "text/plain")}), ""},
		"unknown media type":                  {obj(map[string]any{"d": docProp(float64(1024), canaryType)}), canaryType},
		"application/json":                    {obj(map[string]any{"d": docProp(float64(1024), "application/json")}), "application/json"},
		"maxLength set":                       {obj(map[string]any{"d": withMaxLen}), ""},
		"not a string":                        {obj(map[string]any{"d": map[string]any{"type": "object", "x-untrusted-document": map[string]any{"max_bytes": float64(10), "media_type": "text/plain"}}}), ""},
		"in items":                            {obj(map[string]any{"a": map[string]any{"type": "array", "maxItems": float64(2), "items": docProp(float64(1024), "text/plain")}}), ""},
		"in a nested object":                  {obj(map[string]any{"o": obj(map[string]any{"d": docProp(float64(1024), "text/plain")})}), ""},
		"a third document":                    {obj(map[string]any{"a": docProp(float64(10), "text/plain"), "b": docProp(float64(10), "text/plain"), "c": docProp(float64(10), "text/plain")}), ""},
		"over the 512 KiB sum":                {obj(map[string]any{"a": docProp(float64(262144), "text/plain"), "b": docProp(float64(262144), "text/plain"), "c0": map[string]any{"type": "boolean"}}), ""},
		"a bad property name":                 {obj(map[string]any{"Design-Doc": docProp(float64(1024), "text/plain")}), ""},
		"an unknown key":                      {obj(map[string]any{"d": map[string]any{"type": "string", "x-untrusted-document": map[string]any{"max_bytes": float64(10), "media_type": "text/plain", "charset": "latin1"}}}), "latin1"},
		"a declaration that is not a mapping": {obj(map[string]any{"d": map[string]any{"type": "string", "x-untrusted-document": true}}), ""},
	}
	for name, c := range cases {
		err := inputBroker(c.schema).Validate()
		if name == "over the 512 KiB sum" {
			// 2 × 262,144 = 524,288 is exactly the cap: allowed. The sum rule
			// is exercised with a third byte below.
			if err != nil {
				t.Errorf("%s: two documents at the cap were refused: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), ruleDocument.Text) || !strings.Contains(err.Error(), "rule document") {
			t.Errorf("%s: the refusal does not name the document rule: %v", name, err)
		}
		if c.canary != "" && strings.Contains(err.Error(), c.canary) {
			t.Errorf("%s: the refusal echoes the declaration value %q: %v", name, c.canary, err)
		}
	}
}

// §18.1: the documents of one workflow together carry at most 524,288
// bytes. Unreachable with two documents at the per-document cap, so the
// constant is pinned directly and the walker's sum is checked.
func TestBrokerDocument_TotalCap(t *testing.T) {
	if BrokerDocumentsMax != 2 || BrokerDocumentMaxBytes*BrokerDocumentsMax != BrokerDocumentsTotalMaxBytes { // the rule text says "two"
		t.Fatalf("caps drifted: %d × %d != %d", BrokerDocumentMaxBytes, BrokerDocumentsMax, BrokerDocumentsTotalMaxBytes)
	}
	w := &brokerSchemaWalker{}
	w.docBytes = BrokerDocumentsTotalMaxBytes
	if err := w.addDocument("input_schema.properties.x", BrokerDocument{Property: "x", MaxBytes: 1, MediaType: "text/plain"}); err == nil {
		t.Fatal("a document over the total was accepted")
	}
}

// §18.2: the values a delegate (or a schedule) passes are checked against the
// declaration, and the refusal names the property and the rule, never the
// value.
func TestBrokerDocument_CheckValues(t *testing.T) {
	b := inputBroker(obj(map[string]any{"design": docProp(float64(32), "text/markdown")}))
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := b.CheckDocumentValues(map[string]any{"design": "# fine\nshort"}); err != nil {
		t.Fatalf("a document within bounds was refused: %v", err)
	}
	if err := b.CheckDocumentValues(map[string]any{}); err != nil {
		t.Fatalf("an absent optional document was refused: %v", err)
	}
	const canary = "CANARY-7514"
	bad := map[string]string{
		"over the bound": canary + strings.Repeat("x", 40),
		"a NUL":          canary + "\x00",
		"invalid UTF-8":  canary + "\xff",
	}
	for name, v := range bad {
		err := b.CheckDocumentValues(map[string]any{"design": v})
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "inputs/design") || !strings.Contains(err.Error(), "document") {
			t.Errorf("%s: the refusal does not name the property and rule: %v", name, err)
		}
		if strings.Contains(err.Error(), canary) {
			t.Errorf("%s: the refusal echoes the value: %v", name, err)
		}
	}
}

// §18.7 F2: the published document rule and the rewritten typed-inputs rule.
func TestBrokerDocument_PublishedRules(t *testing.T) {
	byID := map[string]string{}
	for _, r := range BrokerInputRules() {
		byID[r.ID] = r.Text
	}
	doc := byID["document"]
	for _, want := range []string{"x-untrusted-document", "text/plain", "text/markdown", "text/x-diff", "from 1 to 262,144", "two per workflow", "524,288", "^[a-z][a-z0-9_]{0,31}$"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the document rule lacks %q: %q", want, doc)
		}
	}
	if strings.Contains(doc, "application/json") {
		t.Error("the document rule still names application/json (§18.7 F7)")
	}
	typed := byID["typed_inputs_only"]
	if !strings.Contains(typed, "x-untrusted-document") || !strings.Contains(typed, "never inlined") {
		t.Errorf("typed_inputs_only was not rewritten (§18.7 F2): %q", typed)
	}
}

// §18.7 F6: the document key is not passed to the JSON Schema compiler as
// vocabulary, so the compiled input schema accepts the document string.
func TestBrokerDocument_CompiledSchemaAcceptsTheString(t *testing.T) {
	s, err := CompileBrokerSchema("doc", obj(map[string]any{"design": docProp(float64(1024), "text/plain")}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(map[string]any{"design": "text"}); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// §18.1: a schedule's fixed inputs obey the document bounds too.
func TestBrokerDocument_ScheduleInputsChecked(t *testing.T) {
	b := inputBroker(obj(map[string]any{"design": docProp(float64(4), "text/plain")}))
	b.Schedule = &BrokerSchedule{Cron: "0 8 * * *", Inputs: map[string]any{"design": "too long"}}
	err := b.Validate()
	if err == nil || !strings.Contains(err.Error(), "inputs/design") {
		t.Fatalf("a schedule's oversize document was accepted: %v", err)
	}
	if strings.Contains(err.Error(), "too long") {
		t.Fatalf("the refusal echoes the value: %v", err)
	}
}
