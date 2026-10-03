package registry

import (
	"strings"
	"testing"
)

// Agent-administered design §18.2 (2026-10-02): an agent learned each input
// rule from a refusal. BrokerInputRules publishes the rules, and the
// validator's refusals carry the same texts, so the two are one source.
// Both directions (review d94f F9): every published rule is enforced, and
// every refusal names a published rule.

func inputBroker(schema map[string]any) *WorkflowBroker {
	return &WorkflowBroker{InputSchema: schema,
		Egress: BrokerEgress{Output: "result.json", Schema: map[string]any{"type": "object"}}}
}

func obj(props map[string]any) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": props}
}

func TestBrokerInputRules_EveryRuleIsEnforcedAndEveryRefusalNamesOne(t *testing.T) {
	str := func(extra map[string]any) map[string]any {
		m := map[string]any{"type": "string"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	violations := map[string]map[string]any{
		"keywords":   obj(map[string]any{"a": map[string]any{"oneOf": []any{}}}),
		"type":       obj(map[string]any{"a": map[string]any{}}),
		"object":     {"type": "object", "properties": map[string]any{}},
		"array":      obj(map[string]any{"a": map[string]any{"type": "array", "items": str(map[string]any{"enum": []any{"x"}})}}),
		"string":     obj(map[string]any{"a": str(nil)}),
		"untrusted":  obj(map[string]any{"a": str(map[string]any{"x-untrusted": true, "maxLength": float64(513)})}),
		"budget":     obj(map[string]any{"a": map[string]any{"type": "array", "maxItems": float64(50), "items": str(map[string]any{"x-untrusted": true, "maxLength": float64(512)})}}),
		"pattern":    obj(map[string]any{"a": str(map[string]any{"pattern": "^a$"})}),
		"format":     obj(map[string]any{"a": str(map[string]any{"format": "email"})}),
		"untrustmax": obj(map[string]any{"a": str(map[string]any{"x-untrusted": true})}),
		// Broker design §18.7 F2: the document rule joins the published set.
		"document": obj(map[string]any{"a": str(map[string]any{"x-untrusted-document": map[string]any{"max_bytes": float64(0), "media_type": "text/plain"}})}),
	}
	rules := BrokerInputRules()
	byID := map[string]BrokerInputRule{}
	for _, r := range rules {
		byID[r.ID] = r
	}
	for _, r := range rules {
		if r.ID == "typed_inputs_only" {
			continue // enforced at delegate, not by the schema walker
		}
		found := false
		for _, schema := range violations {
			err := inputBroker(schema).Validate()
			if err != nil && strings.Contains(err.Error(), r.Text) {
				found = true
			}
		}
		if !found {
			t.Errorf("rule %q is published but no violation produced its text: %q", r.ID, r.Text)
		}
	}
	for name, schema := range violations {
		err := inputBroker(schema).Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		named := false
		for _, r := range rules {
			if strings.Contains(err.Error(), r.Text) {
				named = true
			}
		}
		if !named {
			t.Errorf("%s: refusal names no published rule: %v", name, err)
		}
	}
	if byID["typed_inputs_only"].Text == "" {
		t.Error("the delegate-time rule (typed inputs only, no attachments) is not published")
	}
}
