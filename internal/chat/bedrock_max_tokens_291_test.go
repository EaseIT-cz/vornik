package chat

// GitHub #291 (implementation review review-20261008-5860 finding 3): Bedrock
// let a role's max_tokens replace the route cap outright, so a role could raise
// its output above the ceiling the operator set for the route, a cost-control
// bypass every other client closed in #100. Bedrock now sends
// EffectiveMaxTokens(ctx, routeCap) like them. LLD 09-agent-runtime-contract.md
// §8.4, 2026-10-13 amendment.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

func bedrockSentMaxTokens(t *testing.T, routeCap, reqMax int) int32 {
	t.Helper()
	p := &BedrockProvider{model: "zai.glm-5", region: "us-east-1", maxTokens: routeCap}
	ctx := WithRequestMaxTokens(context.Background(), reqMax)
	in, err := p.buildConverseInput(ctx, []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if in.InferenceConfig == nil {
		t.Fatalf("no InferenceConfig (cap %d, request %d)", routeCap, reqMax)
	}
	return aws.ToInt32(in.InferenceConfig.MaxTokens)
}

// #291: a role value above the route cap sends the cap (was 9000).
func TestBedrock_RequestMaxTokensClampedToRouteCap_291(t *testing.T) {
	if got := bedrockSentMaxTokens(t, 4096, 9000); got != 4096 {
		t.Errorf("maxTokens = %d, want the route cap 4096 (a role must not raise it)", got)
	}
}

// #291: a role value below the route cap is sent (the role may lower it).
func TestBedrock_RequestMaxTokensBelowRouteCapIsSent_291(t *testing.T) {
	if got := bedrockSentMaxTokens(t, 4096, 1024); got != 1024 {
		t.Errorf("maxTokens = %d, want the role value 1024", got)
	}
}

// #291: with no route cap the role value is sent unchanged.
func TestBedrock_RequestMaxTokensWithoutRouteCapIsSent_291(t *testing.T) {
	if got := bedrockSentMaxTokens(t, 0, 9000); got != 9000 {
		t.Errorf("maxTokens = %d, want the role value 9000 (no route cap)", got)
	}
}

// #291: the json_schema strict-tool branch of buildConverseInput clamps too
// (it calls applyMaxTokens on its own, separately from the default branch).
func TestBedrock_JSONSchemaPathRequestMaxTokensClampedToRouteCap_291(t *testing.T) {
	p := &BedrockProvider{model: "zai.glm-5", region: "us-east-1", maxTokens: 4096}
	ctx := WithRequestMaxTokens(context.Background(), 9000)
	ctx = WithRequestResponseFormatStruct(ctx, &ResponseFormat{Type: "json_schema", JSONSchema: &ResponseJSONSchema{
		Name:   "writer_result",
		Schema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
	}})
	in, err := p.buildConverseInput(ctx, []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if in.ToolConfig == nil {
		t.Fatal("json_schema branch not taken: no ToolConfig")
	}
	if in.InferenceConfig == nil {
		t.Fatal("json_schema path has no InferenceConfig")
	}
	if got := aws.ToInt32(in.InferenceConfig.MaxTokens); got != 4096 {
		t.Errorf("json_schema path maxTokens = %d, want the route cap 4096", got)
	}
}

// recordingStreamBedrockClient records the ConverseStreamInput and fails both
// calls, so CompleteWithToolsStream returns after building the request.
type recordingStreamBedrockClient struct {
	stream *bedrockruntime.ConverseStreamInput
}

func (c *recordingStreamBedrockClient) Converse(context.Context, *bedrockruntime.ConverseInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	return nil, errors.New("recording client: no converse")
}

func (c *recordingStreamBedrockClient) ConverseStream(_ context.Context, in *bedrockruntime.ConverseStreamInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	c.stream = in
	return nil, errors.New("recording client: no stream")
}

// #291: the streaming request clamps the same way as the non-streaming one.
func TestBedrockStream_RequestMaxTokensClampedToRouteCap_291(t *testing.T) {
	for _, tc := range []struct {
		name          string
		routeCap, req int
		want          int32
	}{
		{"above cap sends the cap", 4096, 9000, 4096},
		{"below cap sends the role value", 4096, 1024, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &recordingStreamBedrockClient{}
			p := &BedrockProvider{model: "zai.glm-5", region: "us-east-1", maxTokens: tc.routeCap, client: fc}
			ctx := WithRequestMaxTokens(context.Background(), tc.req)
			_, _ = p.CompleteWithToolsStream(ctx, []Message{{Role: "user", Content: "hi"}}, nil, func(string) {})
			if fc.stream == nil {
				t.Fatal("ConverseStream was not called")
			}
			if fc.stream.InferenceConfig == nil {
				t.Fatal("stream request has no InferenceConfig")
			}
			if got := aws.ToInt32(fc.stream.InferenceConfig.MaxTokens); got != tc.want {
				t.Errorf("stream maxTokens = %d, want %d", got, tc.want)
			}
		})
	}
}
