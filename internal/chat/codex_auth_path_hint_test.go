package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 2026-11-14: with codex_subscription.auth_path set, the quarantine said
// "run `codex login`", but codex login writes ~/.codex/auth.json, which this
// daemon does not read; two logins never reached it (subscription-auth.md,
// amendment 2026-11-14). The error now names the file it reads and, for a
// separate file, the copy that hands a login over.
func TestCodexQuarantine_NamesTheFileItReads_AuthPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	own := filepath.Join(t.TempDir(), "codex-auth.json")
	for _, tc := range []struct {
		path      string
		wantCopy  bool
		wantInMsg string
	}{
		{own, true, own},
		{filepath.Join(home, ".codex", "auth.json"), false, "run `codex login`"},
		{"", false, "run `codex login`"}, // empty auth_path: the CLI default
	} {
		m := newCodexAuthManager(tc.path)
		if tc.path == "" {
			tc.path = m.path // resolved to the default
		}
		m.cached = &codexAuth{}
		m.dead, m.deadRefreshToken, m.deadReason = true, "dead", "oauth/token 401: reused"
		_ = os.MkdirAll(filepath.Dir(tc.path), 0o700)
		_ = os.WriteFile(tc.path, []byte(`{}`), 0o600) // unreadable as a login: no recovery
		_, _, err := m.Token(context.Background())
		if err == nil {
			t.Fatalf("%s: a quarantined token must fail", tc.path)
		}
		msg := err.Error()
		if !strings.Contains(msg, tc.wantInMsg) {
			t.Errorf("%s: message %q lacks %q", tc.path, msg, tc.wantInMsg)
		}
		if got := strings.Contains(msg, "copy ~/.codex/auth.json to"); got != tc.wantCopy {
			t.Errorf("%s: copy hint present=%v, want %v: %q", tc.path, got, tc.wantCopy, msg)
		}
	}
}
