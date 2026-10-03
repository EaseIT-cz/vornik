package chat

import (
	"strings"
	"testing"
)

// Pins what the agent's output-cap rule (LLD 09 §8.4) relies on for this
// route: a tool_use block cut off by max_tokens never reaches content_block_stop,
// so it is not added to the response's tool calls, and finish_reason stays
// "length". The "tool_calls" override below the stop_reason switch applies only
// to calls that completed. Incident 2026-10-03,
// task_20261003135506_c7bec90cfe425d9e (the length-capped critic).
func TestParseMessagesSSE_MaxTokensWithInflightToolUseIsLength(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_01","model":"m","usage":{"input_tokens":50,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool_1","name":"file_write","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.md\",\"content\":\"half a fi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":8192}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	resp, err := parseClaudeMessagesSSE(strings.NewReader(sse), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.Choices[0].Message.ToolCalls); n != 0 {
		t.Fatalf("a cut-off tool_use block surfaced as %d tool call(s); its arguments are incomplete", n)
	}
	if got := resp.Choices[0].FinishReason; got != "length" {
		t.Fatalf("finish_reason = %q, want \"length\" for stop_reason=max_tokens with a tool_use in flight", got)
	}
}
