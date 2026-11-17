package cli

// GitHub #489 (18-security-and-tenancy.md, amendment 2026-11-15): `vornikctl
// key list` printed every row, 497 dead per-task keys beside 4 live ones on
// the reference host's assistant project; it now prints live keys and says
// how many it hid, and `--all` prints every row. `vornikctl key remove`
// deletes a retired key. Found at: production (operator, 2026-10-21).
// Regression test at: integration (the CLI against an HTTP server).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKeyList489_HidesRetiredKeysUnlessAll(t *testing.T) {
	revoked := "2026-11-10T00:00:00Z"
	expired := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(keyListResponse{Keys: []keyListEntry{
			{ID: "k-live", Name: "laptop", KeyPrefix: "p1", CreatedAt: "2026-11-01", ExpiresAt: &future},
			{ID: "k-revoked", Name: "agent:task_x", KeyPrefix: "p2", CreatedAt: "2026-11-02", RevokedAt: &revoked},
			{ID: "k-expired", Name: "old", KeyPrefix: "p3", CreatedAt: "2026-11-03", ExpiresAt: &expired},
		}})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "assistant"
	out, err := captureStdoutFunc(t, func() error { return runKeyList(keyListCmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "k-live") || strings.Contains(out, "k-revoked") || strings.Contains(out, "k-expired") {
		t.Errorf("the default list should show only the live key:\n%s", out)
	}
	if !strings.Contains(out, "2 retired keys (revoked or expired) are hidden; --all shows them") {
		t.Errorf("the default list should say what it hid:\n%s", out)
	}
	keyListAll = true
	out, _ = captureStdoutFunc(t, func() error { return runKeyList(keyListCmd, nil) })
	for _, want := range []string{"k-live", "k-revoked", "revoked", "k-expired", "expired"} {
		if !strings.Contains(out, want) {
			t.Errorf("--all lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "are hidden") {
		t.Errorf("--all should hide nothing:\n%s", out)
	}
}

func TestKeyRemove489(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/v1/projects/assistant/keys/k-gone/remove":
			_ = json.NewEncoder(w).Encode(map[string]string{"removed": "k-gone"})
		case "/api/v1/projects/assistant/keys/k-live/remove":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "KEY_LIVE", "message": "This key is still live. Revoke it first."}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "assistant"
	out, err := captureStdoutFunc(t, func() error { return runKeyRemove(keyRemoveCmd, []string{"k-gone"}) })
	if err != nil || !strings.Contains(out, "Removed key k-gone.") {
		t.Errorf("remove a retired key: %q, %v", out, err)
	}
	_, err = captureStdoutFunc(t, func() error { return runKeyRemove(keyRemoveCmd, []string{"k-live"}) })
	if err == nil || !strings.Contains(err.Error(), "still live") {
		t.Errorf("remove a live key: err %v, want the API's still-live message", err)
	}
}
