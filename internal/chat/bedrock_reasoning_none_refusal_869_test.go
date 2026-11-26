package chat

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go"
)

// GitHub #869, reasoning control design B17: Bedrock's GLM 5.3 accepts
// reasoning_effort but refuses the value "none" with a ValidationException
// that does not name the field. With reasoning off on every model
// (2026-11-21) each call failed, the model's circuit opened, and the 38
// roles that fall back to it lost their fallback. An off call refused this
// way is retried once with "low", and the model is remembered so the next
// off call sends "low" at once.
//
// Found at: production (the B5 Bedrock probe). Regression test at: unit.
// Lower because: the retry is decided in the provider, and a test cannot
// reach Bedrock.

const bedrockNoneRefusal = "The model returned the following errors: Unsupported value: 'none' is not supported with this model. Supported values are: 'low', 'medium', 'high', 'xhigh', and 'max'."

// noneRefusingBedrockClient refuses reasoning_effort "none" the way
// global.zai.glm-5.3 did, and records the value each call carried.
type noneRefusingBedrockClient struct {
	sent []string
}

func sentEffort(in *bedrockruntime.ConverseInput) string {
	return effortOf(in.AdditionalModelRequestFields)
}

// effortOf reads reasoning_effort from the document as the wire carries it:
// marshalled to JSON, which is what Bedrock receives.
func effortOf(doc interface{ MarshalSmithyDocument() ([]byte, error) }) string {
	if doc == nil {
		return ""
	}
	raw, err := doc.MarshalSmithyDocument()
	if err != nil {
		return "unreadable: " + err.Error()
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "unreadable: " + err.Error()
	}
	s, _ := m["reasoning_effort"].(string)
	return s
}

func (c *noneRefusingBedrockClient) Converse(_ context.Context, in *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	var effort string
	if in.AdditionalModelRequestFields != nil {
		effort = sentEffort(in)
	}
	c.sent = append(c.sent, effort)
	if effort == "none" {
		return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: bedrockNoneRefusal}
	}
	return nil, errors.New("stop here: accepted")
}

func (c *noneRefusingBedrockClient) ConverseStream(_ context.Context, in *bedrockruntime.ConverseStreamInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	var effort string
	if in.AdditionalModelRequestFields != nil {
		effort = effortOf(in.AdditionalModelRequestFields)
	}
	c.sent = append(c.sent, effort)
	if effort == "none" {
		return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: bedrockNoneRefusal}
	}
	return nil, errors.New("stop here: accepted")
}

func TestBedrockNoneRefusal_OffIsRetriedAsLow_869(t *testing.T) {
	resetReasoningRefusals()
	resetReasoningOffAsLow()
	fake := &noneRefusingBedrockClient{}
	p := &BedrockProvider{model: "global.zai.glm-5.3", region: "eu-central-1", client: fake}
	off := WithReasoningEffort(context.Background(), ReasoningEffortOff)

	in := &bedrockruntime.ConverseInput{}
	applyReasoningEffort(off, in)
	if _, err := p.converseWithReasoning(off, in); err == nil || err.Error() != "stop here: accepted" {
		t.Fatalf("want the low retry accepted, got %v", err)
	}
	if len(fake.sent) != 2 || fake.sent[0] != "none" || fake.sent[1] != "low" {
		t.Fatalf("sent %q, want none then low", fake.sent)
	}

	// Remembered: the next off call sends low at once.
	in2 := &bedrockruntime.ConverseInput{}
	applyReasoningEffort(off, in2)
	_, _ = p.converseWithReasoning(off, in2)
	if len(fake.sent) != 3 || fake.sent[2] != "low" {
		t.Fatalf("a remembered none refusal must send low at once; sent %q", fake.sent)
	}

	// A level the model takes is sent unchanged.
	high := WithReasoningEffort(context.Background(), ReasoningEffortHigh)
	in3 := &bedrockruntime.ConverseInput{}
	applyReasoningEffort(high, in3)
	_, _ = p.converseWithReasoning(high, in3)
	if len(fake.sent) != 4 || fake.sent[3] != "high" {
		t.Fatalf("high must be sent as high; sent %q", fake.sent)
	}
}

func TestBedrockNoneRefusal_StreamIsRetriedAsLow_869(t *testing.T) {
	resetReasoningRefusals()
	resetReasoningOffAsLow()
	fake := &noneRefusingBedrockClient{}
	p := &BedrockProvider{model: "global.zai.glm-5.3", region: "eu-central-1", client: fake}
	off := WithReasoningEffort(context.Background(), ReasoningEffortOff)
	in := &bedrockruntime.ConverseStreamInput{}
	conv := &bedrockruntime.ConverseInput{}
	applyReasoningEffort(off, conv)
	in.AdditionalModelRequestFields = conv.AdditionalModelRequestFields
	if _, err := p.converseStreamWithReasoning(off, in); err == nil || err.Error() != "stop here: accepted" {
		t.Fatalf("want the low retry accepted, got %v", err)
	}
	if len(fake.sent) != 2 || fake.sent[0] != "none" || fake.sent[1] != "low" {
		t.Fatalf("sent %q, want none then low", fake.sent)
	}
}

func TestBedrockNoneRefusal_OnlyTheNoneValue_869(t *testing.T) {
	if !isBedrockNoneRefusal(&smithy.GenericAPIError{Code: "ValidationException", Message: bedrockNoneRefusal}) {
		t.Fatal("the GLM 5.3 message is a none refusal")
	}
	for _, msg := range []string{
		"Unsupported value: 'low' is not supported with this model.",
		"messages: too long",
	} {
		if isBedrockNoneRefusal(&smithy.GenericAPIError{Code: "ValidationException", Message: msg}) {
			t.Errorf("%q is not a none refusal", msg)
		}
	}
	if isBedrockNoneRefusal(errors.New(bedrockNoneRefusal)) {
		t.Error("only a ValidationException is a none refusal")
	}
}

func resetReasoningOffAsLow() {
	reasoningOffAsLow.Range(func(k, _ any) bool { reasoningOffAsLow.Delete(k); return true })
}
