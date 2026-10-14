package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestClaudeToken_ALockRefusalIsCountedApartFromAFailedRefresh — code review
// b20f F2 (GitHub #142 §S7 #10): a refresh refused because another process
// held the lock must be distinguishable from a refresh the endpoint failed.
func TestClaudeToken_ALockRefusalIsCountedApartFromAFailedRefresh(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	path := writeClaudeCredsTokens(t, t.TempDir(), "old-access", "old-refresh", time.Now().Add(-time.Hour))
	holdAuthLock(t, path)
	m := newChatTestMetrics(t)
	mgr := newClaudeAuthManager(path)
	mgr.tokenURL = "http://127.0.0.1:1" // never reached
	mgr.setMetrics(m)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := mgr.Token(ctx); err == nil {
		t.Fatal("want the refusal")
	}
	if got := testutil.ToFloat64(m.SubscriptionTokenRefreshTotal.WithLabelValues("claude", "lock_timeout")); got != 1 {
		t.Errorf("lock_timeout = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.SubscriptionTokenRefreshTotal.WithLabelValues("claude", "failure")); got != 0 {
		t.Errorf("failure = %v, want 0: a lock refusal is not a failed refresh", got)
	}
}

// TestCodexToken_ALockRefusalIsCounted — the same for codex, whose refresh
// outcomes are otherwise recorded inside doRefreshPOST, which a refusal never
// reaches.
func TestCodexToken_ALockRefusalIsCounted(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: buildCodexJWT(t, "acct-1", time.Now().Add(-time.Hour)), AccessToken: "old",
		RefreshToken: "old-refresh", AccountID: "acct-1",
	})
	holdAuthLock(t, path)
	m := newChatTestMetrics(t)
	mgr := newCodexAuthManager(path)
	mgr.tokenURL = "http://127.0.0.1:1"
	mgr.setMetrics(m)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := mgr.Token(ctx); err == nil {
		t.Fatal("want the refusal")
	}
	if got := testutil.ToFloat64(m.SubscriptionTokenRefreshTotal.WithLabelValues("codex", "lock_timeout")); got != 1 {
		t.Errorf("lock_timeout = %v, want 1", got)
	}
	// doRefreshPOST never ran, so the refusal is counted once, not also as a
	// failure (code review 6ad1 #3).
	if got := testutil.ToFloat64(m.SubscriptionTokenRefreshTotal.WithLabelValues("codex", "failure")); got != 0 {
		t.Errorf("failure = %v, want 0", got)
	}
}

// TestCtxMutex_UnlockOfAnUnlockedMutexPanics — code review b20f F1: a stray
// Unlock must fail loudly, as sync.Mutex does, not block its caller (and every
// later Token call) forever.
func TestCtxMutex_UnlockOfAnUnlockedMutexPanics(t *testing.T) {
	var l ctxMutex
	defer func() {
		if recover() == nil {
			t.Error("Unlock of an unlocked ctxMutex did not panic")
		}
	}()
	l.Unlock()
}

// TestCtxMutex_LockContextAndUnlockPair — the ordinary path, and a waiter
// that gives up leaves the lock as it found it.
func TestCtxMutex_LockContextAndUnlockPair(t *testing.T) {
	var l ctxMutex
	if err := l.LockContext(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.LockContext(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("second LockContext on a held lock = %v, want context.Canceled", err)
	}
	l.Unlock()
	if err := l.LockContext(context.Background(), "x"); err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	l.Unlock()
}
