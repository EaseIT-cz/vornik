package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// Issue #329 (audit 2026-10-12, dangling-backlog sweep): the "bedrock: converse
// completed" debug line carried openai_messages_in and bedrock_messages_sent but
// nothing to explain the gap, so an operator could not tell a legitimate
// parallel-tool-result aggregation from a dropped tool result (the 2026-05-07
// I1 bug class). Design: chat-bedrock-conversion.md "Tool-result counters".
func TestBedrockConversionLog_ToolResultCounters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bedrockCovConverseJSON("done", "end_turn", 1, 1, 2)))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	p := &BedrockProvider{
		model: "m329", region: "us-east-1",
		client: bedrockCovRuntimeClient(t, srv.URL),
		logger: zerolog.New(&buf).Level(zerolog.DebugLevel),
	}
	// system, user, assistant with two parallel calls, two tool results, then a
	// user nudge that collapses into the tool-result turn.
	msgs := []Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "look both up"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "a", Type: "function", Function: FunctionCall{Name: "f", Arguments: `{}`}},
			{ID: "b", Type: "function", Function: FunctionCall{Name: "f", Arguments: `{}`}},
		}},
		{Role: "tool", ToolCallID: "a", Content: "ra"},
		{Role: "tool", ToolCallID: "b", Content: "rb"},
		{Role: "user", Content: "now answer"},
	}
	tools := []Tool{{Type: "function", Function: ToolFunction{
		Name: "f", Parameters: json.RawMessage(`{"type":"object"}`),
	}}}
	if _, err := p.CompleteWithTools(context.Background(), msgs, tools); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	line := bedrock329Line(t, &buf)
	want := map[string]float64{
		"openai_messages_in":     6,
		"bedrock_messages_sent":  3, // user, assistant, user(2 toolResult + text)
		"system_msgs_in":         1,
		"tool_msgs_in":           2,
		"tool_result_blocks_out": 2,
		"tool_results_merged":    1,
	}
	for k, v := range want {
		got, ok := line[k].(float64)
		if !ok || got != v {
			t.Errorf("%s = %v, want %v (line: %v)", k, line[k], v, line)
		}
	}

	// A tool-free turn flattens tool blocks to text (regression 2026-08-16), so
	// the out-counters read zero by design and the design says to skip check 1.
	buf.Reset()
	if _, err := p.Complete(context.Background(), msgs); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	line = bedrock329Line(t, &buf)
	if line["tool_result_blocks_out"] != float64(0) || line["tools_offered"] != float64(0) {
		t.Errorf("tool-free turn: want tool_result_blocks_out 0 and tools_offered 0, got %v", line)
	}
}

func bedrock329Line(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["message"] == "bedrock: converse completed" {
			line = m
		}
	}
	if line == nil {
		t.Fatalf("no converse-completed line in:\n%s", buf.String())
	}
	return line
}
