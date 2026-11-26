package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// GitHub #869: no agent role could turn a model's reasoning off or down.
// The companion reviewer's GLM 5.3 spent its whole output cap on reasoning,
// every call, and nine reviews failed (2026-11-20). These pin the first two
// links of the fix: the context keeps "off", and the OpenAI-compatible client
// (http, openrouter, ollama_cloud, vertex) sends the level.
//
// Found at: production (companion reviews failed on the output cap).
// Regression test at: unit. Lower because: the setting is decided and written
// here; the production comparison is the design's B8 last item.

func TestWithReasoningEffort_KeepsOff_869(t *testing.T) {
	ctx := WithReasoningEffort(context.Background(), ReasoningEffortOff)
	if got := ReasoningEffortFromContext(ctx); got != ReasoningEffortOff {
		t.Fatalf("ReasoningEffortFromContext = %q, want %q", got, ReasoningEffortOff)
	}
	if got := ReasoningEffortFromContext(WithReasoningEffort(context.Background(), "maximum")); got != "" {
		t.Fatalf("an unknown level must still be dropped, got %q", got)
	}
}

func TestOpenAICompatible_SendsReasoningEffort_869(t *testing.T) {
	for _, tc := range []struct {
		level, want string
		present     bool
	}{
		{ReasoningEffortLow, "low", true},
		{ReasoningEffortHigh, "high", true},
		{ReasoningEffortOff, "none", true}, // OpenAI-compatible spelling of off
		{"", "", false},                    // unset sends nothing, as today
	} {
		var captured map[string]any
		srv := captureWireBody(t, func(_ *http.Request, b []byte) { captured = decodeWireMap(t, b) })
		c := NewClient(srv.URL, "k", "glm-5.3-flash")
		ctx := context.Background()
		if tc.level != "" {
			ctx = WithReasoningEffort(ctx, tc.level)
		}
		if _, err := c.Complete(ctx, []Message{{Role: "user", Content: "hi"}}); err != nil {
			srv.Close()
			t.Fatalf("level %q: Complete: %v", tc.level, err)
		}
		srv.Close()
		got, ok := captured["reasoning_effort"]
		if ok != tc.present || (ok && got != tc.want) {
			t.Errorf("level %q: reasoning_effort = %v (present %v), want %q (present %v)", tc.level, got, ok, tc.want, tc.present)
		}
	}
}

// The stream path builds its own request literal; before #869 it would have
// dropped a reasoning level the non-stream path sends.
func TestOpenAICompatibleStream_SendsReasoningEffort_869(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := &Client{endpoint: srv.URL, model: "glm-5.3-flash", httpClient: &http.Client{}}
	ctx := WithReasoningEffort(context.Background(), ReasoningEffortOff)
	_, _ = c.CompleteWithToolsStream(ctx, []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if captured == nil {
		t.Fatal("upstream never received a request body")
	}
	if got := captured["reasoning_effort"]; got != "none" {
		t.Errorf("stream reasoning_effort = %v, want \"none\"", got)
	}
}

// Bedrock forwarded the context's level verbatim. With "off" now on the
// context it would have sent reasoning_effort "off", which is no provider's
// spelling; it sends "none", as the OpenAI-compatible routes do (#869).
func TestBedrock_OffIsSentAsNone_869(t *testing.T) {
	input := &bedrockruntime.ConverseInput{}
	applyReasoningEffort(WithReasoningEffort(context.Background(), ReasoningEffortOff), input)
	if input.AdditionalModelRequestFields == nil {
		t.Fatal("off did not reach AdditionalModelRequestFields")
	}
	raw, err := input.AdditionalModelRequestFields.(interface{ MarshalSmithyDocument() ([]byte, error) }).MarshalSmithyDocument()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["reasoning_effort"] != "none" {
		t.Errorf("bedrock reasoning_effort = %v, want \"none\"", got["reasoning_effort"])
	}
}
