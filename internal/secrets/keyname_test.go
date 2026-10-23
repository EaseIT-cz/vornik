package secrets

import "testing"

// security review 2026-10-16 (#344, 5d-E): a credential under a secret-named
// JSON key has no value shape the detector knows, so the key name decides.
// Moved from subjectstore with the rule (GitHub #395): the export, the
// task_messages metadata walk and the chat-audit args scrub share it.
func TestIsSecretKeyName(t *testing.T) {
	for _, k := range []string{"password", "Password", "api_key", "apiKey", "X-Auth-Token",
		"Authorization", "client_secret", "accessToken", "session_id", "privateKey", "cookie",
		// review c2d9 F1: plurals and compounds.
		"tokens", "secrets", "passwords", "cookies", "apiKeys", "api_keys", "access_tokens",
		"accessTokens", "refresh_token", "credentials"} {
		if !IsSecretKeyName(k) {
			t.Errorf("IsSecretKeyName(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"author_id", "max_tokens", "maxTokens", "prompt_tokens", "total_tokens",
		"reply_to_message_id", "chat_id", "address", "note", "passage", "keyboard", "name"} {
		if IsSecretKeyName(k) {
			t.Errorf("IsSecretKeyName(%q) = true, want false", k)
		}
	}
}

// GitHub #395: the chat-audit args scrub's two hand-listed keys are covered by
// the shared rule, so replacing the list loses neither.
func TestIsSecretKeyName_CoversTheRetiredArgsList(t *testing.T) {
	for _, k := range []string{"approval_token", "daemon_auth"} {
		if !IsSecretKeyName(k) {
			t.Errorf("IsSecretKeyName(%q) = false; the retired sensitiveToolArgKeys list named it", k)
		}
	}
}
