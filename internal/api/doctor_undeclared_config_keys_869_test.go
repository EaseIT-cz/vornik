package api

import (
	"strings"
	"testing"
)

// GitHub #869, reasoning control design B7: the doctor lists every daemon
// config key the daemon does not read. chat.thinking_enabled read as "thinking
// off" for five months after it was removed; nothing said it did nothing.
//
// Found at: production (the operator's live config). Regression test at:
// unit. Lower because: the check's verdict is decided here.
func TestDoctor_UndeclaredConfigKeys_869(t *testing.T) {
	h := &DoctorHandlers{}
	if got := h.checkUndeclaredConfigKeys(); got.Status != "SKIPPED" {
		t.Fatalf("not wired: status %s, want SKIPPED", got.Status)
	}
	h.SetUndeclaredConfigKeys(nil)
	if got := h.checkUndeclaredConfigKeys(); got.Status != "OK" {
		t.Fatalf("none: status %s, want OK", got.Status)
	}
	h.SetUndeclaredConfigKeys([]string{"auth.use_chain", "chat.thinking_enabled"})
	got := h.checkUndeclaredConfigKeys()
	if got.Status != "WARNING" || !strings.Contains(got.Message, "2") || len(got.Items) != 2 || got.Items[0] != "auth.use_chain" {
		t.Fatalf("two keys: %+v", got)
	}
}
