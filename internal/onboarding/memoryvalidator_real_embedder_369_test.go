package onboarding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GitHub #369 item 5 (outcome-quality §S8.10): through the REAL embedder (not a
// stubbed probe, which is what the other tests here use) a rejected key and a
// dimension mismatch both came back as (nil, nil), so the validator called a
// rejected key "unreachable" and a wrong embedding_dimension a blocking outage;
// its embedding_key_rejected and advisory dimension_mismatch branches were dead.

func realValidatorAgainst(t *testing.T, h http.HandlerFunc, dim int) MemoryValidationResult {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewMemoryValidator().Validate(context.Background(), MemoryConfigProposal{
		Enabled:            true,
		EmbeddingEndpoint:  srv.URL,
		EmbeddingModel:     "m",
		EmbeddingAPIKey:    "wrong",
		EmbeddingDimension: dim,
	})
}

func TestMemoryValidate369_RealEmbedderRejectedKeyIsNamed(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		res := realValidatorAgainst(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }, 1024)
		if res.EmbeddingOK || len(res.Failures) != 1 || res.Failures[0].Name != "embedding_key_rejected" {
			t.Fatalf("HTTP %d: %+v, want one embedding_key_rejected failure", code, res)
		}
	}
	res := realValidatorAgainst(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, 1024)
	if len(res.Failures) != 1 || res.Failures[0].Name != "embedding_unreachable" {
		t.Fatalf("HTTP 502: %+v, want embedding_unreachable", res)
	}
}

func TestMemoryValidate369_RealEmbedderDimensionMismatchIsAdvisory(t *testing.T) {
	res := realValidatorAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}]}`))
	}, 1024)
	if !res.EmbeddingOK || res.ReturnedDimension != 3 || res.DimensionMatches {
		t.Fatalf("%+v: want reachable, returned dimension 3, not matching", res)
	}
	if len(res.Failures) != 1 || res.Failures[0].Name != "dimension_mismatch" || res.Failures[0].Severity != "advisory" {
		t.Fatalf("failures %+v, want one advisory dimension_mismatch", res.Failures)
	}
}
