package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// config_template_drift acknowledgements (drift design, slice C): the daemon
// writes, admin-gated and fail-closed.

func ackTree(t *testing.T) string {
	return driftTree(t, map[string]string{
		".templates/.stamp": "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", ".templates/.classes": driftClasses,
		".templates/workflows/w.md": "a\nfix\n", "workflows/w.md": "a\n",
		".templates/workflows/clean.md": "c\n", "workflows/clean.md": "c\n",
	})
}

func ackCall(t *testing.T, h *DoctorHandlers, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor/ack", bytes.NewBufferString(body))
	req = withAdminKeyContext(req, key)
	rec := httptest.NewRecorder()
	h.AckDoctorFinding(rec, req)
	return rec
}

func ackHandler(dir string) *DoctorHandlers {
	h := driftHandler(dir, "1111111aaaaa")
	h.server = NewServer(adminAuthOpts()...)
	return h
}

func TestAckDoctorFinding_RecordsAndTheRowQuietens(t *testing.T) {
	dir := ackTree(t)
	h := ackHandler(dir)
	rec := ackCall(t, h, "sk-admin", `{"check":"config_template_drift","file":"workflows/w.md"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out ackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Recorded) != 1 || out.Recorded[0].Class != "hard" {
		t.Fatalf("response %s (%v)", rec.Body, err)
	}
	got := h.checkConfigTemplateDrift()
	if got.Status != "OK" || !strings.Contains(got.Message, "1 finding(s) acknowledged") {
		t.Fatalf("after the ack: %s %q %v", got.Status, got.Message, got.Items)
	}
}

func TestAckDoctorFinding_Refusals(t *testing.T) {
	dir := ackTree(t)
	h := ackHandler(dir)
	for _, tc := range []struct {
		name, key, body string
		want            int
	}{
		{"non-admin key", "sk-someone", `{"check":"config_template_drift","file":"workflows/w.md"}`, http.StatusForbidden},
		{"other check", "sk-admin", `{"check":"workflow_onfail_masking","file":"workflows/w.md"}`, http.StatusBadRequest},
		{"path escape", "sk-admin", `{"check":"config_template_drift","file":"../../etc/passwd"}`, http.StatusBadRequest},
		{"baseline artifact", "sk-admin", `{"check":"config_template_drift","file":".templates/workflows/w.md"}`, http.StatusBadRequest},
		{"nothing to ack", "sk-admin", `{"check":"config_template_drift","file":"workflows/clean.md"}`, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := ackCall(t, h, tc.key, tc.body); rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, ".template-acks")); !os.IsNotExist(err) {
		t.Fatal("a refused ack wrote the store")
	}
}

// No server wired → refused, never an unauthenticated write.
func TestAckDoctorFinding_FailsClosedWithoutAServer(t *testing.T) {
	h := driftHandler(ackTree(t), "1111111aaaaa")
	if rec := ackCall(t, h, "sk-admin", `{"check":"config_template_drift","file":"workflows/w.md"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

// A stale baseline (not this binary's revision) refuses: keys would describe
// findings the row does not show.
func TestAckDoctorFinding_StaleBaselineRefused(t *testing.T) {
	h := ackHandler(ackTree(t))
	h.buildRevision = func() (string, bool, bool) { return "3333333ccccc", false, true }
	if rec := ackCall(t, h, "sk-admin", `{"check":"config_template_drift","file":"workflows/w.md"}`); rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
}

// The read-only report never writes: store and journal byte-identical after it.
func TestConfigTemplateDrift_TheReportNeverWrites(t *testing.T) {
	dir := ackTree(t)
	h := ackHandler(dir)
	if rec := ackCall(t, h, "sk-admin", `{"check":"config_template_drift","file":"workflows/w.md"}`); rec.Code != http.StatusOK {
		t.Fatalf("ack: %d", rec.Code)
	}
	// A finding disappears (the fix is taken) — a report must NOT prune it.
	if err := os.WriteFile(filepath.Join(dir, "workflows/w.md"), []byte("a\nfix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, ".template-acks"))
	jb, _ := os.ReadFile(filepath.Join(dir, ".origin/.acks.journal"))
	_ = h.checkConfigTemplateDrift()
	after, _ := os.ReadFile(filepath.Join(dir, ".template-acks"))
	ja, _ := os.ReadFile(filepath.Join(dir, ".origin/.acks.journal"))
	if !bytes.Equal(before, after) || !bytes.Equal(jb, ja) {
		t.Fatal("the doctor report mutated the acknowledgement store or journal")
	}
}

// The journal-only sentence reaches the row.
func TestConfigTemplateDrift_AckMissingSentence(t *testing.T) {
	dir := ackTree(t)
	h := ackHandler(dir)
	if rec := ackCall(t, h, "sk-admin", `{"check":"config_template_drift","file":"workflows/w.md"}`); rec.Code != http.StatusOK {
		t.Fatalf("ack: %d", rec.Code)
	}
	if err := os.Remove(filepath.Join(dir, ".template-acks")); err != nil {
		t.Fatal(err)
	}
	got := h.checkConfigTemplateDrift()
	if got.Status != "WARNING" || !strings.Contains(strings.Join(got.Items, "\n"), "acknowledgement missing from the store") {
		t.Fatalf("%s %v", got.Status, got.Items)
	}
}

// A tree with NO baseline says so — not "not current" — and names where it
// looked, once (the live probe on 2026-09-24 printed the advice twice).
func TestAckDoctorFinding_NoBaselineSaysSo(t *testing.T) {
	dir := driftTree(t, map[string]string{"workflows/w.md": "a\n"})
	rec := ackCall(t, ackHandler(dir), "sk-admin", `{"check":"config_template_drift","file":"workflows/w.md"}`)
	body := rec.Body.String()
	if rec.Code != http.StatusConflict || !strings.Contains(body, "NO_BASELINE") || !strings.Contains(body, dir) {
		t.Fatalf("%d %s", rec.Code, body)
	}
	if strings.Count(body, "install-config-assets") != 1 {
		t.Errorf("the advice appears %d times: %s", strings.Count(body, "install-config-assets"), body)
	}
}
