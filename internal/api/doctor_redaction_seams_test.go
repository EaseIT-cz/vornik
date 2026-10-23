package api

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/auditredact"
)

// GitHub #395: the content-store seams' bypass was counted nowhere and reported
// nowhere. The check states the state AND the denominator for every store,
// zero rows included (a store missing from the line would read as clean).
func TestDoctorRedactionSeams(t *testing.T) {
	stores := []string{auditredact.StoreChatAuditLog, auditredact.StoreChatSystemPrompts, auditredact.StoreStepPrompts, auditredact.StoreTaskMessages}
	census := func() map[string]auditredact.SeamCount {
		return map[string]auditredact.SeamCount{
			auditredact.StoreTaskMessages: {Total: 7, Skipped: 7},
			auditredact.StoreChatAuditLog: {Total: 3, Redacted: 1, Scanned: 2},
		}
	}
	cases := []struct {
		name       string
		set        bool
		reason     auditredact.Reason
		stores     []string
		wantStatus string
		wantIn     []string
	}{
		{name: "scanning", set: true, reason: auditredact.ReasonNone, stores: stores, wantStatus: "OK",
			wantIn: []string{"scanned before persist", "chat_audit_log 3 rows (1 redacted, 0 unscanned)", "step_prompts 0 rows"}},
		{name: "secrets disabled", set: true, reason: auditredact.ReasonSecretsDisabled, stores: stores, wantStatus: "WARNING",
			wantIn: []string{"secrets.enabled=false", "task_messages 7 rows (0 redacted, 7 unscanned)", "vornik_redaction_seam_rows_total"}},
		{name: "detector failed", set: true, reason: auditredact.ReasonDetectorUnavailable, stores: stores, wantStatus: "ERROR",
			wantIn: []string{"failed to construct", "task_messages 7 rows"}},
		{name: "no detector", set: true, reason: auditredact.ReasonDetectorNil, stores: stores, wantStatus: "WARNING",
			wantIn: []string{"reason=detector_nil"}},
		{name: "no stores", set: true, reason: auditredact.ReasonNone, stores: nil, wantStatus: "SKIPPED", wantIn: []string{"no content store"}},
		{name: "no snapshot", set: false, wantStatus: "SKIPPED", wantIn: []string{"skipping"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			if tc.set {
				h.SetRedactionSeams(tc.reason, tc.stores, census)
			}
			got := h.checkRedactionSeams()
			if got.Name != "redaction_seams" || got.Status != tc.wantStatus {
				t.Fatalf("got %s/%s, want redaction_seams/%s: %s", got.Name, got.Status, tc.wantStatus, got.Message)
			}
			for _, w := range tc.wantIn {
				if !strings.Contains(got.Message, w) {
					t.Errorf("message lacks %q: %s", w, got.Message)
				}
			}
		})
	}
	// A nil census still renders every store, at zero.
	h := &DoctorHandlers{}
	h.SetRedactionSeams(auditredact.ReasonNone, stores, nil)
	if got := h.checkRedactionSeams(); !strings.Contains(got.Message, "task_messages 0 rows") {
		t.Errorf("nil census: %s", got.Message)
	}
}
