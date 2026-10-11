package agentadmin

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func jwtWith(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + enc.EncodeToString(raw) + "." + enc.EncodeToString([]byte("sig"))
}

// GitHub #119 item 8 (2026-10-08): a provider identity is the verified email
// of an OAuth sign-in's id_token, and nothing else (agent-administered design
// §22.2.7): email_verified must be true, and the address must have a shape
// that cannot close the phrase it sits in.
func TestEmailFromIDToken(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want string
	}{
		"no token":           {"", ""},
		"not a JWT":          {"abc", ""},
		"bad payload":        {"a.!!!.c", ""},
		"no email":           {jwtWith(t, map[string]any{"sub": "1", "email_verified": true}), ""},
		"unverified":         {jwtWith(t, map[string]any{"email": "alice@corp.example", "email_verified": false}), ""},
		"verified absent":    {jwtWith(t, map[string]any{"email": "alice@corp.example"}), ""},
		"verified as string": {jwtWith(t, map[string]any{"email": "alice@corp.example", "email_verified": "true"}), ""},
		"bracket":            {jwtWith(t, map[string]any{"email": "a)b@corp.example", "email_verified": true}), ""},
		"semicolon":          {jwtWith(t, map[string]any{"email": "a;b@corp.example", "email_verified": true}), ""},
		"space":              {jwtWith(t, map[string]any{"email": "a b@corp.example", "email_verified": true}), ""},
		"newline":            {jwtWith(t, map[string]any{"email": "a@corp.example\nok", "email_verified": true}), ""},
		"oversize":           {jwtWith(t, map[string]any{"email": strings.Repeat("a", 65) + "@corp.example", "email_verified": true}), ""},
		"valid":              {jwtWith(t, map[string]any{"email": "alice+work@corp.example", "email_verified": true}), "alice+work@corp.example"},
	}
	for name, c := range cases {
		got, ok := EmailFromIDToken(c.raw)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: got %q %v, want %q", name, got, ok, c.want)
		}
	}
}
