package chat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// Issue #328 (audit 2026-10-12, dangling-backlog sweep): Bedrock provider errors
// were wrapped with fmt.Errorf and never typed as *GatewayError, so the
// dispatcher's prune-and-retry (which matches only *GatewayError + Retryable())
// never fired for Bedrock, not even on ThrottlingException or
// ServiceUnavailableException. Design: chat-bedrock-conversion.md
// "Provider error typing (amended 2026-10-13, #328)".

// bedrock328Provider points a real SDK client at a server that answers every
// Converse with the given status and AWS error type. Retries are disabled so
// the SDK's own backoff does not slow the test or mask the mapping.
func bedrock328Provider(t *testing.T, status int, errType string) *BedrockProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Amzn-Errortype", errType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"upstream said no"}`))
	}))
	t.Cleanup(srv.Close)
	client := bedrockruntime.New(bedrockruntime.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRET", ""),
		BaseEndpoint: aws.String(srv.URL),
		Retryer:      aws.NopRetryer{},
	})
	return &BedrockProvider{model: "m328", region: "us-east-1", client: client}
}

func TestBedrockGatewayError_RetryableStatusesAreTyped(t *testing.T) {
	for _, tc := range []struct {
		status  int
		errType string
	}{
		{http.StatusTooManyRequests, "ThrottlingException"},
		{http.StatusServiceUnavailable, "ServiceUnavailableException"},
		{http.StatusInternalServerError, "InternalServerException"},
	} {
		t.Run(tc.errType, func(t *testing.T) {
			p := bedrock328Provider(t, tc.status, tc.errType)
			_, err := p.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			var ge *GatewayError
			if !errors.As(err, &ge) {
				t.Fatalf("err = %T %v, want a *GatewayError in the chain", err, err)
			}
			if ge.Status != tc.status || !ge.Retryable() {
				t.Errorf("Status = %d Retryable = %v, want %d / true", ge.Status, ge.Retryable(), tc.status)
			}
			// Top of the chain, so the type-assertion consumers (llmjudge,
			// memory/graph) recognise it too.
			if _, ok := err.(*GatewayError); !ok {
				t.Errorf("err is %T, want *GatewayError at the top of the chain", err)
			}
			// The old text survives for every substring matcher.
			if !strings.Contains(err.Error(), "bedrock Converse(m328)") || !strings.Contains(err.Error(), tc.errType) {
				t.Errorf("err text lost the provider context: %q", err.Error())
			}
			// The AWS error is still reachable.
			var sc interface{ HTTPStatusCode() int }
			if !errors.As(err, &sc) || sc.HTTPStatusCode() != tc.status {
				t.Errorf("original AWS response error not reachable through Unwrap")
			}
		})
	}
}

func TestBedrockGatewayError_ClientErrorIsTypedButNotRetryable(t *testing.T) {
	p := bedrock328Provider(t, http.StatusBadRequest, "ValidationException")
	_, err := p.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	var ge *GatewayError
	if !errors.As(err, &ge) {
		t.Fatalf("err = %v, want *GatewayError", err)
	}
	if ge.Status != http.StatusBadRequest || ge.Retryable() {
		t.Errorf("Status = %d Retryable = %v, want 400 / false", ge.Status, ge.Retryable())
	}
}

// A context-overflow 400 must stay non-infra after typing (the 2026-07-12
// incident class): IsContextOverflow reads the preserved message text.
func TestBedrockGatewayError_ContextOverflowStillNotInfra(t *testing.T) {
	aws400 := errors.New("operation error Bedrock Runtime: Converse, https response error StatusCode: 400, ValidationException: Input is too long for requested model")
	err := bedrockGatewayError("Converse", "m328", statusErr{error: aws400, code: 400})
	if !IsContextOverflow(err) {
		t.Errorf("IsContextOverflow(%q) = false, want true", err)
	}
	if IsUpstreamInfraError(err) {
		t.Errorf("a context-overflow 400 classified as infra")
	}
}

type statusErr struct {
	error
	code int
}

func (e statusErr) HTTPStatusCode() int { return e.code }
func (e statusErr) Unwrap() error       { return e.error }

func TestBedrockGatewayError_MidStreamExceptionsAreTyped(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{&bedrocktypes.ThrottlingException{Message: aws.String("slow down")}, 429},
		{&bedrocktypes.ServiceUnavailableException{Message: aws.String("down")}, 503},
		{&bedrocktypes.InternalServerException{Message: aws.String("oops")}, 500},
	} {
		err := bedrockGatewayError("ConverseStream", "m328", fmt.Errorf("stream: %w", tc.err))
		var ge *GatewayError
		if !errors.As(err, &ge) || ge.Status != tc.status {
			t.Errorf("%T: err = %v, want *GatewayError status %d", tc.err, err, tc.status)
			continue
		}
		if !strings.Contains(err.Error(), "bedrock ConverseStream(m328)") {
			t.Errorf("err text = %q, want the ConverseStream prefix", err.Error())
		}
	}
}

func TestBedrockGatewayError_UntypedErrorsKeepOldWrapping(t *testing.T) {
	for _, in := range []error{context.Canceled, context.DeadlineExceeded, errors.New("dial tcp: no such host")} {
		err := bedrockGatewayError("Converse", "m328", in)
		var ge *GatewayError
		if errors.As(err, &ge) {
			t.Errorf("%v: typed as GatewayError, want the plain wrapping", in)
		}
		if !errors.Is(err, in) || err.Error() != "bedrock Converse(m328): "+in.Error() {
			t.Errorf("%v: got %q, want the unchanged fmt.Errorf wrapping", in, err)
		}
	}
}
