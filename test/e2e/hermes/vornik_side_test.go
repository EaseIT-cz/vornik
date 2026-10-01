//go:build e2e_hermes

package hermes

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// waitResult polls result until the task is complete.
func waitResult(t *testing.T, s *stack, taskID string) map[string]any {
	t.Helper()
	var out map[string]any
	waitFor(t, "task "+taskID, 5*time.Minute, func() bool {
		text, isErr := mcpCall(t, s.apiURL, s.brokerKey, "result", map[string]any{"task_id": taskID, "wait_seconds": 20})
		if isErr {
			t.Fatalf("result %s: %s", taskID, text)
		}
		out = map[string]any{}
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("result %s: %v: %s", taskID, err, text)
		}
		return out["complete"] == true
	})
	return out
}

func delegate(t *testing.T, s *stack, workflow string, inputs map[string]any) string {
	t.Helper()
	text, isErr := mcpCall(t, s.apiURL, s.brokerKey, "delegate", map[string]any{"workflow": workflow, "inputs": inputs})
	if isErr {
		t.Fatalf("delegate %s: %s", workflow, text)
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.TaskID == "" {
		t.Fatalf("delegate %s: %v: %s", workflow, err, text)
	}
	return out.TaskID
}

// TestVornikSide is the model-free smoke of the Vornik half: a broker task
// runs its (scripted) agent for real and returns only the declared egress.
func TestVornikSide(t *testing.T) {
	s := startStack(t)
	task := delegate(t, s, "mail-digest", map[string]any{"since": "2026-09-29T00:00:00Z"})
	res := waitResult(t, s, task)
	raw, _ := json.Marshal(res)
	if res["status"] != "COMPLETED" || res["egress_error"] != nil {
		t.Fatalf("result = %s\nscript failures: %v", raw, s.llm.Failures())
	}
	if !strings.Contains(string(raw), digestLine) {
		t.Fatalf("the digest did not come back: %s", raw)
	}
	if strings.Contains(string(raw), rawMailSentinel) {
		t.Fatalf("raw mail crossed the egress boundary: %s", raw)
	}
	if n := len(s.mailRead.Calls()); n < 2 {
		t.Fatalf("the agent made %d mail-read calls, want at least 2", n)
	}
}
