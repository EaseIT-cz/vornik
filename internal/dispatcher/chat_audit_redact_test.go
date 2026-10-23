package dispatcher

import "testing"

func TestRedactSensitiveToolArgs(t *testing.T) {
	// approval_token redacted, other fields preserved.
	in := `{"mode":"submit","submission_id":"s1","approval_token":"deadbeefdeadbeef"}`
	out := redactSensitiveToolArgs(in)
	if want := `"[REDACTED:secret_field]"`; !contains(out, want) {
		t.Fatalf("token not redacted: %s", out)
	}
	if contains(out, "deadbeefdeadbeef") {
		t.Fatalf("raw token leaked: %s", out)
	}
	if !contains(out, `"submission_id":"s1"`) && !contains(out, `"submission_id": "s1"`) {
		t.Fatalf("submission_id lost: %s", out)
	}
	// empty token: unchanged (nothing to redact).
	if got := redactSensitiveToolArgs(`{"approval_token":""}`); contains(got, "[REDACTED") {
		t.Fatalf("empty token should not be redacted: %s", got)
	}
	// non-JSON: returned unchanged.
	if got := redactSensitiveToolArgs("not json"); got != "not json" {
		t.Fatalf("non-json mutated: %s", got)
	}
}

// GitHub #395: the hand-kept two-key list became secrets.IsSecretKeyName, at
// any depth — daemon_auth still matches, and a nested password no longer
// escapes (chat-audit retention and redaction design §9.2.5).
func TestRedactSensitiveToolArgs_SharedKeyRuleAtAnyDepth(t *testing.T) {
	in := `{"daemon_auth":"cap-1234","to":"bob","smtp":{"password":"hunter2-hunter2"},"list":[{"apiKey":"k-999999"}],"max_tokens":12}`
	out := redactSensitiveToolArgs(in)
	for _, leaked := range []string{"cap-1234", "hunter2-hunter2", "k-999999"} {
		if contains(out, leaked) {
			t.Fatalf("%q leaked: %s", leaked, out)
		}
	}
	if !contains(out, `"to":"bob"`) || !contains(out, `"max_tokens":12`) {
		t.Fatalf("innocuous fields lost: %s", out)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
