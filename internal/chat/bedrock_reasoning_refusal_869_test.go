package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go"
)

// GitHub #869, reasoning control design B4: on Bedrock a model that refuses
// reasoning_effort answers ValidationException. The call is retried once
// without the field and the refusal remembered, as on the OpenAI-compatible
// routes, so a role set to off keeps working.
//
// Found at: production (design review of the fix for the 2026-11-20
// companion-review failures). Regression test at: unit. Lower because: the
// retry is decided in the provider.
type refusingBedrockClient struct {
	calls   int
	carried []bool
}

func (c *refusingBedrockClient) Converse(_ context.Context, in *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	c.calls++
	has := in.AdditionalModelRequestFields != nil
	c.carried = append(c.carried, has)
	if has {
		return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "The model does not support the parameter reasoning_effort"}
	}
	return nil, errors.New("stop here: the call without the field was made")
}

func (c *refusingBedrockClient) ConverseStream(context.Context, *bedrockruntime.ConverseStreamInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	return nil, errors.New("not used")
}

func TestBedrockReasoningRefusal_RetriedWithoutTheField_869(t *testing.T) {
	resetReasoningRefusals()
	fake := &refusingBedrockClient{}
	p := &BedrockProvider{model: "zai.glm-5.3", region: "us-east-1", client: fake}
	in := &bedrockruntime.ConverseInput{}
	applyReasoningEffort(WithReasoningEffort(context.Background(), ReasoningEffortOff), in)
	_, err := p.converseWithReasoning(context.Background(), in)
	if err == nil || err.Error() != "stop here: the call without the field was made" {
		t.Fatalf("want the retry's own error, got %v", err)
	}
	if fake.calls != 2 || !fake.carried[0] || fake.carried[1] {
		t.Fatalf("calls %d carried %v, want the field then none", fake.calls, fake.carried)
	}
	// Remembered: the next call drops the field before sending.
	in2 := &bedrockruntime.ConverseInput{}
	applyReasoningEffort(WithReasoningEffort(context.Background(), ReasoningEffortLow), in2)
	_, _ = p.converseWithReasoning(context.Background(), in2)
	if fake.calls != 3 || fake.carried[2] {
		t.Fatalf("a remembered refusal must not resend the field; carried %v", fake.carried)
	}
}

func TestBedrockReasoningRefusal_OtherValidationErrorIsReturned_869(t *testing.T) {
	resetReasoningRefusals()
	other := &smithy.GenericAPIError{Code: "ValidationException", Message: "messages: too long"}
	if isBedrockReasoningRefusal(other) {
		t.Fatal("a ValidationException that does not name the field is not a refusal")
	}
	if !isBedrockReasoningRefusal(&smithy.GenericAPIError{Code: "ValidationException", Message: "extraneous key [reasoning_effort] is not permitted"}) {
		t.Fatal("a ValidationException naming the field is a refusal")
	}
}
