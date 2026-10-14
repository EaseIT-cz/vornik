package chat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// holdAuthLock takes "<path>.lock" through its own open file description, as
// a sibling process holding the credential-refresh lock would.
func holdAuthLock(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	})
}

func awaitErr(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s is still waiting after its context ended", what)
		return nil
	}
}

// TestClaudeToken_RefreshLockWaitEndsWithTheCaller — audit 2026-10-12, GitHub
// #142 §S7 #10: the refresh's cross-process flock was a blocking LOCK_EX with
// no bound, so a sibling that held it made Token wait forever, deaf to the
// caller's context.
func TestClaudeToken_RefreshLockWaitEndsWithTheCaller(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	path := writeClaudeCredsTokens(t, t.TempDir(), "old-access", "old-refresh", time.Now().Add(-time.Hour))
	holdAuthLock(t, path)
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("the refresh POSTed without holding the lock")
	}))
	defer ts.Close()
	mgr := newClaudeAuthManager(path)
	mgr.tokenURL = ts.URL

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := mgr.Token(ctx); done <- err }()
	err := awaitErr(t, done, "Token (claude)")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Token error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), ".credentials.json.lock") {
		t.Errorf("Token error %q does not name the lock file", err)
	}
}

// TestCodexToken_RefreshLockWaitEndsWithTheCaller — the same for codex.
func TestCodexToken_RefreshLockWaitEndsWithTheCaller(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: buildCodexJWT(t, "acct-1", time.Now().Add(-time.Hour)), AccessToken: "old",
		RefreshToken: "old-refresh", AccountID: "acct-1",
	})
	holdAuthLock(t, path)
	mgr := newCodexAuthManager(path)
	mgr.tokenURL = "http://127.0.0.1:1" // never reached

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := mgr.Token(ctx); done <- err }()
	if err := awaitErr(t, done, "Token (codex)"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Token error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// TestClaudeToken_CallersBehindARefreshCanGiveUp — audit 2026-10-12, GitHub
// #142 §S7 #10: Token held a sync.Mutex across the whole refresh, so every
// other model call through the provider queued behind it with no way to give
// up. A caller whose context ends now returns instead of queueing.
func TestClaudeToken_CallersBehindARefreshCanGiveUp(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	path := writeClaudeCredsTokens(t, t.TempDir(), "old-access", "old-refresh", time.Now().Add(-time.Hour))
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		http.Error(w, "late", http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	defer close(release)
	mgr := newClaudeAuthManager(path)
	mgr.tokenURL = ts.URL

	// The holder: a refresh in flight, holding m.mu.
	go func() { _, _ = mgr.Token(context.Background()) }()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := mgr.Token(ctx); done <- err }()
	err := awaitErr(t, done, "a Token call queued behind a refresh")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Token error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "waiting for the claude credential refresh") {
		t.Errorf("Token error %q does not say what it was waiting for", err)
	}
}
