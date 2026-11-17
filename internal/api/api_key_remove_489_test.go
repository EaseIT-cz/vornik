package api

// GitHub #489 (18-security-and-tenancy.md, amendment 2026-11-15 R2):
// POST /api/v1/projects/{p}/keys/{kid}/remove deletes a retired key at once
// and records who did; a live key answers 409 and another project's key 404.
// Found at: production (operator, 2026-10-21: dead keys could not be
// removed). Regression test at: integration (the HTTP handler). A database
// key is refused on the route by C3 (keyRoutes613 lists it).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func TestRemoveAPIKey489(t *testing.T) {
	repo := &memAPIKeyRepo{}
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	for _, k := range []*persistence.APIKey{
		{ID: "akey-gone", ProjectID: "assistant", Name: "gone", KeyHash: "h1", KeyPrefix: "sk-vornik-g", CreatedAt: now, RevokedAt: &past},
		{ID: "akey-live", ProjectID: "assistant", Name: "live", KeyHash: "h2", KeyPrefix: "sk-vornik-l", CreatedAt: now},
		{ID: "akey-b", ProjectID: "project-b", Name: "b", KeyHash: "h3", KeyPrefix: "sk-vornik-b", CreatedAt: now, RevokedAt: &past},
	} {
		_ = repo.Create(context.Background(), k)
	}
	audit := &stubAdminAuditRepo{}
	s := newAPIKeyServer(repo)
	s.adminAuditRepo = audit
	call := func(project, id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.RemoveRetiredAPIKey(rec, httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project+"/keys/"+id+"/remove", nil), id)
		return rec
	}
	if rec := call("assistant", "akey-gone"); rec.Code != http.StatusOK {
		t.Fatalf("remove a revoked key: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := repo.GetByID(context.Background(), "akey-gone"); err == nil {
		t.Fatal("the removed key is still stored")
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "key.remove" || audit.rows[0].Target != "akey-gone" || audit.rows[0].Source != "api" {
		t.Fatalf("want one key.remove row, got %+v", audit.rows)
	}
	raw, _ := json.Marshal(audit.rows[0])
	if !strings.Contains(string(raw), "gone") || strings.Contains(string(raw), "sk-vornik") || strings.Contains(string(raw), `"h1`) {
		t.Errorf("the audit row should name the key and carry no prefix or hash: %s", raw)
	}
	if rec := call("assistant", "akey-live"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "still live") {
		t.Errorf("remove a live key: %d %s, want 409 still live", rec.Code, rec.Body.String())
	}
	if rec := call("assistant", "akey-b"); rec.Code != http.StatusNotFound {
		t.Errorf("remove another project's key: %d, want 404", rec.Code)
	}
	if rec := httptest.NewRecorder(); true {
		s.RemoveRetiredAPIKey(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/assistant/keys/akey-b/remove", nil), "akey-b")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET: %d, want 405", rec.Code)
		}
	}
	if len(audit.rows) != 1 {
		t.Errorf("a refused remove wrote an audit row: %+v", audit.rows)
	}
}

// Code review 149d F1: with no audit sink nothing is removed.
func TestRemoveAPIKey489_RefusedWithoutAudit(t *testing.T) {
	repo := &memAPIKeyRepo{}
	past := time.Now().UTC().Add(-time.Minute)
	_ = repo.Create(context.Background(), &persistence.APIKey{ID: "akey-gone", ProjectID: "assistant", Name: "gone", KeyHash: "h1", KeyPrefix: "sk-vornik-g", CreatedAt: past, RevokedAt: &past})
	s := newAPIKeyServer(repo)
	rec := httptest.NewRecorder()
	s.RemoveRetiredAPIKey(rec, httptest.NewRequest(http.MethodPost, "/api/v1/projects/assistant/keys/akey-gone/remove", nil), "akey-gone")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("remove with no audit: %d, want 503", rec.Code)
	}
	if _, err := repo.GetByID(context.Background(), "akey-gone"); err != nil {
		t.Fatal("a key was removed with no audit record")
	}
}
