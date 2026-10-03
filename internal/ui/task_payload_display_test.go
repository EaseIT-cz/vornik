package ui

import (
	"encoding/json"
	"strings"
	"testing"
)

// Review 20261003-6fec item 3 (broker design §18.3): a broker task's payload
// carries its document inputs in context.broker_inputs, up to 512 KB. The
// task page's "Raw payload" printed it whole. The display keeps every other
// field and shortens each long broker input to a head and its size.
func TestDisplayTaskPayload_TruncatesBrokerInputs(t *testing.T) {
	doc := "# Design\n" + strings.Repeat("x", 300000) + "TAIL-SENTINEL"
	raw, _ := json.Marshal(map[string]any{"taskType": "doc-review", "context": map[string]any{
		"prompt":        "Broker request",
		"broker_inputs": map[string]any{"design": doc, "focus": "security"},
	}})
	out := displayTaskPayload(raw)
	if len(out) > 8192 {
		t.Fatalf("display is %d bytes; the document was not shortened", len(out))
	}
	s := string(out)
	for _, want := range []string{"# Design", "security", "Broker request", "doc-review", "bytes, shown"} {
		if !strings.Contains(s, want) {
			t.Errorf("display lacks %q: %s", want, s)
		}
	}
	if strings.Contains(s, "TAIL-SENTINEL") {
		t.Error("the document's tail was displayed")
	}
	// A payload without broker inputs, or not JSON, is unchanged.
	plain := []byte(`{"context":{"prompt":"hi"}}`)
	if string(displayTaskPayload(plain)) != string(plain) {
		t.Errorf("a plain payload changed: %s", displayTaskPayload(plain))
	}
	if string(displayTaskPayload([]byte("not json"))) != "not json" {
		t.Error("non-JSON changed")
	}
}
