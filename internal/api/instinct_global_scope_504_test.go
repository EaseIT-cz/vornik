package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GitHub #504 (continuous-learning-instinct-layer-design, corrected
// 2026-11-05): a global instinct row copies one project's trigger and action
// verbatim, so it can carry project-identifying content. A project-scoped
// key no longer reads global rows: List omits them, and Show answers 404 as
// for another project's row. Before the fix both returned them to any
// authenticated caller.
//
// Found at: review (designing #502, 2026-10-22), observed at integration (the real handler with a scoped key)
// Regression test at: integration, the real instincts router over httptest with a scoped context.

func TestListInstincts_ScopedKeyGetsNoGlobalRows_504(t *testing.T) {
	s := instinctTestServer(&stubInstinctRepo{rows: sampleInstincts()}, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts", nil)
	req = req.WithContext(ContextWithScopeForTesting(req.Context(), "alpha"))
	rec := httptest.NewRecorder()
	s.ListInstincts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var resp InstinctListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for _, in := range resp.Instincts {
		if in.Scope == "global" {
			t.Fatalf("a project-scoped key was served global row %s", in.ID)
		}
	}
	if len(resp.Instincts) != 2 {
		t.Fatalf("got %d rows, want alpha's 2 project rows", len(resp.Instincts))
	}
}

func TestShowInstinct_ScopedKeyGlobalRowIs404_504(t *testing.T) {
	s := instinctTestServer(&stubInstinctRepo{rows: sampleInstincts()}, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts/ins_3", nil)
	req = req.WithContext(ContextWithScopeForTesting(req.Context(), "alpha"))
	rec := httptest.NewRecorder()
	s.instinctsRouter(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("scoped show of a global row = %d, want 404", rec.Code)
	}
}

// An unscoped caller (admin-class, or a single-tenant install with auth off)
// still reads global rows.
func TestInstincts_UnscopedStillSeesGlobal_504(t *testing.T) {
	s := instinctTestServer(&stubInstinctRepo{rows: sampleInstincts()}, nil, nil)
	rec := httptest.NewRecorder()
	s.instinctsRouter(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instincts/ins_3", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unscoped show of a global row = %d, want 200", rec.Code)
	}
}
