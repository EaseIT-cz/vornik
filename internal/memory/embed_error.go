package memory

import (
	"context"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/ctxerr"
)

// Embed failure reasons, one per remedy (outcome-quality-design.md §S8.10,
// GitHub #369 item 5). Every cause that leaves a caller without a vector
// returns an *EmbedError carrying one of these; none returns (nil, nil).
//
// This is the local form §S8.3 #4 allows: a reason enum owned by the component
// that decides. #4's shared skip-signal primitive does not exist yet; when it
// does, these values are what the embedder adopts into it.
const (
	// EmbedReasonNotConfigured: no endpoint or model (Bedrock: no model or
	// region). Remedy: configure memory.
	EmbedReasonNotConfigured = "not_configured"
	// EmbedReasonBackendInit: the Bedrock client could not be built (the AWS
	// config failed to load). Remedy: region / credentials.
	EmbedReasonBackendInit = "backend_init"
	// EmbedReasonRequest: the request could not be built (marshal, URL).
	EmbedReasonRequest = "request"
	// EmbedReasonTimeout: the call's deadline, or the HTTP client's own
	// timeout, fired. Remedy: a faster endpoint or a longer bound.
	EmbedReasonTimeout = "timeout"
	// EmbedReasonCancelled: the caller went away.
	EmbedReasonCancelled = "cancelled"
	// EmbedReasonTransport: the endpoint could not be reached, or a Bedrock
	// invoke failed. Remedy: network / the provider's health.
	EmbedReasonTransport = "transport"
	// EmbedReasonHTTPStatus: a non-200 reply; Status carries the code (401/403
	// a rejected key, 429/5xx the endpoint).
	EmbedReasonHTTPStatus = "http_status"
	// EmbedReasonReadBody: a 200 whose body could not be read.
	EmbedReasonReadBody = "read_body"
	// EmbedReasonDecode: a body that is not the expected JSON.
	EmbedReasonDecode = "decode"
	// EmbedReasonProviderError: a 200 carrying an error object.
	EmbedReasonProviderError = "provider_error"
	// EmbedReasonMalformedResponse: the same index answered twice.
	EmbedReasonMalformedResponse = "malformed_response"
	// EmbedReasonEmptyVector: a vector with no values.
	EmbedReasonEmptyVector = "empty_vector"
	// EmbedReasonDimensionMismatch: a vector whose width is not the configured
	// one (or not the first one's, when none is configured); Got and Want set.
	// Remedy: embedding_dimension or the model.
	EmbedReasonDimensionMismatch = "dimension_mismatch"
	// EmbedReasonPartialResponse: fewer (or more) vectors than inputs.
	EmbedReasonPartialResponse = "partial_response"
	// EmbedReasonUnsupportedModel: a Bedrock model id that is neither Titan
	// nor Cohere. Remedy: the model id.
	EmbedReasonUnsupportedModel = "unsupported_model"
)

// EmbedError is the embedder's failure: what went wrong, by remedy, and the
// underlying cause when there is one.
type EmbedError struct {
	Reason string
	// Status is the HTTP status for EmbedReasonHTTPStatus, else 0.
	Status int
	// Got and Want are the returned and expected widths for
	// EmbedReasonDimensionMismatch, else 0.
	Got, Want int
	// Detail is a short human-readable addition (the provider's message).
	Detail string
	Err    error
}

func (e *EmbedError) Error() string {
	msg := "embed " + e.Reason
	switch {
	case e.Status != 0:
		msg += fmt.Sprintf(": HTTP %d", e.Status)
	case e.Reason == EmbedReasonDimensionMismatch:
		msg += fmt.Sprintf(": got dim=%d, expected=%d", e.Got, e.Want)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *EmbedError) Unwrap() error { return e.Err }

// EmbedFailureReason returns the reason of an *EmbedError anywhere in err's
// chain.
func EmbedFailureReason(err error) (string, bool) {
	var ee *EmbedError
	if errors.As(err, &ee) {
		return ee.Reason, true
	}
	return "", false
}

func embedFail(reason string, err error) *EmbedError {
	return &EmbedError{Reason: reason, Err: err}
}

// embedCallFail classifies a failed provider call: a spent or cancelled
// context, or an HTTP client timeout, is timeout / cancelled; anything else is
// transport.
func embedCallFail(ctx context.Context, err error) *EmbedError {
	switch ctxerr.Classify(ctx, err) {
	case ctxerr.Timeout:
		return embedFail(EmbedReasonTimeout, err)
	case ctxerr.Cancelled:
		return embedFail(EmbedReasonCancelled, err)
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) && te.Timeout() {
		return embedFail(EmbedReasonTimeout, err)
	}
	return embedFail(EmbedReasonTransport, err)
}

// setMetrics wires the failure counter; nil-safe.
func (e *Embedder) setMetrics(m *Metrics) { e.metrics = m }

// countFailure records one failed Embed / EmbedQuery call. Untyped errors (an
// invalid scope: a programming error, already returned) are not counted.
func (e *Embedder) countFailure(scope EmbedScope, err error) {
	if e == nil || e.metrics == nil || e.metrics.EmbedFailuresTotal == nil {
		return
	}
	reason, ok := EmbedFailureReason(err)
	if !ok {
		return
	}
	e.metrics.EmbedFailuresTotal.WithLabelValues(scope.CallSite, reason).Inc()
}
