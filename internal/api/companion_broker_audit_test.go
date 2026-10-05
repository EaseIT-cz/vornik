package api

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func TestBrokerAuditCopyPreservesNumbers(t *testing.T) {
	want := json.Number("9007199254740993")
	got := deepCopyJSON(map[string]any{"id": want})
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"id":9007199254740993}` {
		t.Fatalf("validated ID changed: %s", b)
	}
}

func TestBrokerAuditValidatedNumbersReachPromptExactly(t *testing.T) {
	wf := &registry.Workflow{ID: "audit", Broker: &registry.WorkflowBroker{
		InputSchema: map[string]any{"type": "object", "additionalProperties": false,
			"properties": map[string]any{"id": map[string]any{"type": "integer"}}},
		Egress: registry.BrokerEgress{Output: "result.json", Schema: map[string]any{"type": "object"}},
	}}
	inputs, err := validateBrokerInputs(wf, json.RawMessage(`{"id":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := renderBrokerPrompt(wf, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `"id": 9007199254740993`) || strings.Contains(prompt, "9007199254740992") {
		t.Fatalf("validated ID changed in prompt: %s", prompt)
	}
}

func TestBrokerAuditNestedUntrustedArrays(t *testing.T) {
	input := map[string]any{"text": []any{[]any{"hostile text"}}}
	wrapUntrustedPath(input, []string{"text[][]"})
	got := input["text"].([]any)[0].([]any)[0].(string)
	if !strings.Contains(got, "<untrusted_content") {
		t.Fatalf("nested input was not wrapped: %s", got)
	}
}

func TestBrokerAuditArraysOfObjects(t *testing.T) {
	for _, path := range [][]string{{"text[]", "body"}, {"text[]", "body[]"}} {
		var leaf any = "hostile text"
		if strings.HasSuffix(path[1], "[]") {
			leaf = []any{leaf}
		}
		obj := map[string]any{"body": leaf}
		input := map[string]any{"text": []any{obj}}
		wrapUntrustedPath(input, path)
		got := obj["body"]
		if arr, ok := got.([]any); ok {
			got = arr[0]
		}
		if !strings.Contains(got.(string), "<untrusted_content") {
			t.Fatalf("unwrapped path %v: %s", path, got)
		}
	}
}

func TestBrokerAuditLiteralPropertyNames(t *testing.T) {
	for _, name := range []string{"a.b", "text[]", "properties", ""} {
		t.Run(name, func(t *testing.T) {
			wf := &registry.Workflow{ID: "audit", Broker: &registry.WorkflowBroker{
				InputSchema: map[string]any{"type": "object", "additionalProperties": false,
					"properties": map[string]any{name: map[string]any{"type": "string", "maxLength": 32, "x-untrusted": true}}},
				Egress: registry.BrokerEgress{Output: "result.json", Schema: map[string]any{"type": "object"}},
			}}
			err := wf.Broker.Validate()
			if strings.ContainsAny(name, ".[]") {
				if err == nil {
					t.Fatal("ambiguous schema property name accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{name: "hostile text"})
			if err != nil {
				t.Fatal(err)
			}
			inputs, err := validateBrokerInputs(wf, raw)
			if err != nil {
				t.Fatal(err)
			}
			prompt, err := renderBrokerPrompt(wf, inputs)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(prompt, `: "hostile text"`) {
				t.Fatalf("literal property was not wrapped: %s", prompt)
			}
		})
	}
}
