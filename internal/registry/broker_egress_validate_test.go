package registry

import (
	"strings"
	"testing"
)

// Broker design §17 (2026-10-03): task task_20261003003901_067399b47e7fc6f2
// lost a whole 3-step run because its summary was 603 characters against
// maxLength 600. One validator now judges an answer for both the executor
// (so the answering step gets one corrective turn) and result().

func egressWF(schema map[string]any, maxBytes int) *Workflow {
	return &Workflow{ID: "ns--p--w", Broker: &WorkflowBroker{
		InputSchema: map[string]any{"type": "object"},
		Egress:      BrokerEgress{Output: "result.json", Schema: schema, MaxBytes: maxBytes},
	}}
}

var summarySchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []any{"summary"},
	"properties": map[string]any{"summary": map[string]any{"type": "string", "maxLength": float64(600)}},
}

func TestValidateBrokerEgress_Verdicts(t *testing.T) {
	wf := egressWF(summarySchema, 0)
	if v := ValidateBrokerEgress(wf, []byte(`{"summary":"fine"}`)); v.Class != "" || v.Doc == nil {
		t.Fatalf("valid answer: %+v", v)
	}
	long := `{"summary":"` + strings.Repeat("x", 603) + `"}`
	v := ValidateBrokerEgress(wf, []byte(long))
	if v.Class != EgressClassSchema || len(v.Messages) != 1 || v.Messages[0] != "/summary: maxLength (600 allowed, 603 written)" {
		t.Fatalf("603-character summary: %+v", v)
	}
	if v := ValidateBrokerEgress(wf, []byte(`{"summary":`)); v.Class != EgressClassSchema {
		t.Fatalf("not JSON: %+v", v)
	}
	if v := ValidateBrokerEgress(wf, []byte(`{}`)); v.Class != EgressClassSchema || !strings.Contains(strings.Join(v.Messages, ";"), "required") {
		t.Fatalf("missing field: %+v", v)
	}
	if v := ValidateBrokerEgress(wf, []byte(`{"summary":"a","extra":"b"}`)); v.Class != EgressClassSchema || !strings.Contains(strings.Join(v.Messages, ";"), "extra") {
		t.Fatalf("extra field: %+v", v)
	}
	small := egressWF(summarySchema, 16)
	if v := ValidateBrokerEgress(small, []byte(`{"summary":"0123456789"}`)); v.Class != EgressClassOversize || v.Bytes != 24 || v.Limit != 16 {
		t.Fatalf("oversize: %+v", v)
	}
}

// Review 97b9 F2: no message carries a value the answer wrote, whatever the
// keyword. The library's own messages quote values for some keywords, so
// they are never used.
func TestValidateBrokerEgress_MessagesCarryNoValues(t *testing.T) {
	const canary = "CANARY7731"
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"email":  map[string]any{"type": "string", "format": "email"},
			"code":   map[string]any{"type": "string", "pattern": "^[0-9]+$"},
			"n":      map[string]any{"type": "number"},
			"kind":   map[string]any{"enum": []any{"a", "b"}},
			"amount": map[string]any{"type": "number", "maximum": float64(10)},
			"fixed":  map[string]any{"const": "x"},
		},
	}
	doc := `{"email":"` + canary + `","code":"` + canary + `","n":"` + canary + `","kind":"` + canary + `","amount":7731,"fixed":"` + canary + `"}`
	v := ValidateBrokerEgress(egressWF(schema, 0), []byte(doc))
	if v.Class != EgressClassSchema || len(v.Messages) < 6 {
		t.Fatalf("want six failures: %+v", v)
	}
	for _, m := range v.Messages {
		if strings.Contains(m, canary) || strings.Contains(m, "7731") {
			t.Errorf("message carries a value: %q", m)
		}
	}
}
