package secrets

import (
	"regexp"
	"strings"
)

// secretKeyWords are the key segments whose value is treated as a credential
// whatever it looks like. A key is split into segments on non-alphanumerics
// and at lower→upper case changes ("accessToken" → access, token), so
// "author_id" or "max_tokens" do not match while "api_key", "apiKey" and
// "X-Auth-Token" do.
var secretKeyWords = map[string]bool{
	"password": true, "passwd": true, "passphrase": true, "pass": true, "pwd": true,
	"secret": true, "token": true, "apikey": true, "auth": true, "authorization": true,
	"bearer": true, "cookie": true, "credential": true, "credentials": true,
	"signature": true, "privatekey": true, "otp": true,
}

// secretKeyPairs are two-segment names that form a credential name.
var secretKeyPairs = map[[2]string]bool{
	{"api", "key"}: true, {"private", "key"}: true, {"access", "key"}: true,
	{"session", "id"}: true, {"session", "key"}: true, {"client", "secret"}: true,
}

var keySegmentSplit = regexp.MustCompile(`[^A-Za-z0-9]+|([a-z0-9])([A-Z])`)

// tokenCountQualifiers precede "token(s)" in a key that counts model tokens
// (max_tokens, prompt_tokens), which is not a credential.
var tokenCountQualifiers = map[string]bool{
	"max": true, "prompt": true, "completion": true, "total": true, "input": true,
	"output": true, "num": true, "cached": true, "reasoning": true,
}

// singular drops one plural "s" so "tokens", "apiKeys" and "passwords" match
// (review c2d9 F1). "pass" and "credentials" are listed as they are.
func singular(s string) string {
	if len(s) > 3 && strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss") {
		return strings.TrimSuffix(s, "s")
	}
	return s
}

// IsSecretKeyName reports whether a (decoded) JSON object key names a
// credential, so the whole value under it is treated as one whatever it looks
// like: the detector's generic_kv rule needs key=value or key: value and cannot
// see a JSON "password": "…" pair.
//
// ONE implementation, shared by every rule that keys on a name: the Art 15
// export (subjectstore, design 2026-07-29 §15.11.4a), the task_messages
// metadata walk and the chat-audit tool-argument scrub (chat-audit design §9,
// GitHub #395). A safety rule with two copies has one that drifts.
func IsSecretKeyName(k string) bool {
	segs := strings.Fields(strings.ToLower(keySegmentSplit.ReplaceAllString(k, "${1} ${2}")))
	for i, raw := range segs {
		s := singular(raw)
		prev := ""
		if i > 0 {
			prev = segs[i-1]
		}
		if s == "token" && tokenCountQualifiers[prev] {
			continue
		}
		if secretKeyWords[raw] || secretKeyWords[s] {
			return true
		}
		if i > 0 && secretKeyPairs[[2]string{prev, s}] {
			return true
		}
	}
	return false
}
