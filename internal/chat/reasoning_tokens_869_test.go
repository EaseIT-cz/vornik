package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

// GitHub #869, reasoning control design B6: a call that spends its cap
// reasoning read as an empty answer, because nothing parsed what the
// provider reported about reasoning. Ollama Cloud returns the text as
// message.reasoning and no reasoning token count (measured 2026-11-21);
// OpenAI-style providers report usage.completion_tokens_details.reasoning_tokens.
//
// Found at: production (companion reviews failed on the output cap with
// content_bytes=0 and nothing else). Regression test at: unit. Lower because:
// the parse is decided here.
func TestChatResponse_ParsesReasoning_869(t *testing.T) {
	ollama := `{"choices":[{"index":0,"message":{"role":"assistant","content":"391","reasoning":"17*23 is 391."},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":20,"completion_tokens":41,"total_tokens":61}}`
	var r ChatResponse
	if err := json.Unmarshal([]byte(ollama), &r); err != nil {
		t.Fatal(err)
	}
	if r.Choices[0].Message.ReasoningContent != "17*23 is 391." {
		t.Errorf("reasoning text = %q", r.Choices[0].Message.ReasoningContent)
	}
	if r.Usage.CompletionTokensDetails != nil {
		t.Errorf("no count reported must stay unknown, got %+v", r.Usage.CompletionTokensDetails)
	}

	openai := `{"choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"thinking..."},"finish_reason":"length"}],
		"usage":{"prompt_tokens":20,"completion_tokens":8192,"total_tokens":8212,"completion_tokens_details":{"reasoning_tokens":8192}}}`
	r = ChatResponse{}
	if err := json.Unmarshal([]byte(openai), &r); err != nil {
		t.Fatal(err)
	}
	if r.Choices[0].Message.ReasoningContent != "thinking..." {
		t.Errorf("reasoning_content = %q", r.Choices[0].Message.ReasoningContent)
	}
	if d := r.Usage.CompletionTokensDetails; d == nil || d.ReasoningTokens != 8192 {
		t.Errorf("reasoning tokens = %+v, want 8192", d)
	}

	// Never echoed upstream: a message carrying reasoning marshals without it.
	out, err := json.Marshal(Message{Role: "assistant", Content: "391", ReasoningContent: "secret thoughts"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "secret thoughts") {
		t.Errorf("reasoning was echoed: %s", out)
	}
}
