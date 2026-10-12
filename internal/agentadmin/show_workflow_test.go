package agentadmin

import (
	"encoding/json"
	"testing"
)

func TestParseShowWorkflowInput(t *testing.T) {
	for _, project := range []string{"finance", "hermes--finance"} {
		in, err := ParseShowWorkflowInput("hermes", json.RawMessage(`{"project":"`+project+`","slug":"digest"}`))
		if err != nil || in.Project != "finance" || in.Slug != "digest" {
			t.Fatalf("%+v %v", in, err)
		}
	}
	for _, raw := range []string{`{}`, `null`, `{"project":"","slug":"digest"}`, `{"project":"codex--finance","slug":"digest"}`, `{"project":"finance","slug":"../digest"}`, `{"project":"finance","slug":"digest","extra":true}`} {
		if _, err := ParseShowWorkflowInput("hermes", json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
