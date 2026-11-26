package chat

import (
	"context"
	"net/http"
	"testing"
)

// GitHub #869, reasoning control design B4: behind an OpenAI-compatible
// endpoint the model's chat template decides, and llama.cpp templates read
// different variables (the Hermes lane measured Gemma 4 and Nemotron reading
// enable_thinking, North reading reasoning). A model configured with
// reasoning_template_variable gets that variable in chat_template_kwargs:
// false for off, true for a level, plus reasoning_effort as usual. A model
// without one is sent as before.
//
// Found at: production (design review: a template that ignores
// reasoning_effort leaves an "off" role reasoning). Regression test at:
// unit. Lower because: the body is built here.
func TestReasoningTemplateVariable_869(t *testing.T) {
	for _, tc := range []struct {
		model, level string
		wantVar      any // nil = no chat_template_kwargs
	}{
		{"gemma4-26b", ReasoningEffortOff, false},
		{"gemma4-26b", ReasoningEffortHigh, true},
		{"gemma4-26b", "", nil}, // no level: nothing added
		{"other", ReasoningEffortOff, nil},
	} {
		var body map[string]any
		srv := captureWireBody(t, func(_ *http.Request, b []byte) { body = decodeWireMap(t, b) })
		c := NewClient(srv.URL, "k", tc.model, WithReasoningTemplateVariables(map[string]string{"gemma4-26b": "enable_thinking"}))
		ctx := context.Background()
		if tc.level != "" {
			ctx = WithReasoningEffort(ctx, tc.level)
		}
		if _, err := c.Complete(ctx, []Message{{Role: "user", Content: "hi"}}); err != nil {
			srv.Close()
			t.Fatal(err)
		}
		srv.Close()
		kw, _ := body["chat_template_kwargs"].(map[string]any)
		switch {
		case tc.wantVar == nil && kw != nil:
			t.Errorf("%s/%q: chat_template_kwargs = %v, want none", tc.model, tc.level, kw)
		case tc.wantVar != nil && (kw == nil || kw["enable_thinking"] != tc.wantVar):
			t.Errorf("%s/%q: chat_template_kwargs = %v, want enable_thinking=%v", tc.model, tc.level, kw, tc.wantVar)
		}
	}
}
