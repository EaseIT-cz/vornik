package chat

// #772 (2026-11-14): the Codex endpoint stopped serving gpt-5.4 and
// gpt-5.4-mini to ChatGPT accounts, so a config that left
// codex_subscription.model empty failed every call with a bare
// "HTTP 400: {...model_not_found...}" and no hint what to change.
// These tests pin the new default and the refusal hint
// (subscription-auth.md, Amendment 2026-11-14 (b)).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexSubscription_EmptyModelDefaultsToCurrentCodexDefault(t *testing.T) {
	if got := NewCodexSubscriptionClient("").Model(); got != "gpt-6-luna" {
		t.Fatalf("empty model = %q, want gpt-6-luna (the Codex CLI default on 2026-11-14)", got)
	}
}

// codexRefusingClient is a client whose auth is valid and whose
// Responses endpoint answers 400 model_not_found, as chatgpt.com did
// for gpt-5.4 on 2026-11-14.
func codexRefusingClient(t *testing.T, cachePath string) *CodexSubscriptionClient {
	t.Helper()
	dir := t.TempDir()
	authPath := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken:      buildCodexJWT(t, "acct-1", time.Now().Add(time.Hour)),
		AccessToken:  buildCodexJWT(t, "acct-1", time.Now().Add(time.Hour)),
		RefreshToken: "rt", AccountID: "acct-1",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account.","type":"invalid_request_error","code":"model_not_found"}}`))
	}))
	t.Cleanup(ts.Close)
	c := NewCodexSubscriptionClient("gpt-5.4", WithCodexSubscriptionAuthPath(authPath))
	c.responsesURL = ts.URL
	c.modelsCachePath = cachePath
	return c
}

func TestCodexSubscription_ModelRefusalNamesTheSettingAndVisibleModels(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "models_cache.json")
	if err := os.WriteFile(cache, []byte(`{"fetched_at":"2026-11-14T10:00:00Z","models":[
		{"slug":"gpt-6-luna","visibility":"list"},
		{"slug":"gpt-5.6-terra","visibility":"list"},
		{"slug":"codex-auto-review","visibility":"hide"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := codexRefusingClient(t, cache).Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"HTTP 400", "model_not_found", "set chat.router.codex_subscription.model to a model this account offers", "gpt-6-luna, gpt-5.6-terra"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "codex-auto-review") {
		t.Errorf("error lists a hidden model:\n%s", msg)
	}
}

func TestCodexSubscription_ModelRefusalWithoutCacheGivesOnlyTheHint(t *testing.T) {
	_, err := codexRefusingClient(t, filepath.Join(t.TempDir(), "absent.json")).Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "set chat.router.codex_subscription.model to a model this account offers") {
		t.Errorf("error lacks the hint:\n%s", msg)
	}
	if strings.Contains(msg, "Codex CLI lists") {
		t.Errorf("no cache, yet the error claims a list:\n%s", msg)
	}
}

func TestCodexModelRefusalHint_OtherErrorsGetNoHint(t *testing.T) {
	if h := codexModelRefusalHint([]byte(`{"error":{"code":"rate_limit_exceeded"}}`), ""); h != "" {
		t.Errorf("hint on a non-model error: %q", h)
	}
}

// The production path passes no cache path: the hint reads the Codex
// CLI's default ~/.codex/models_cache.json.
func TestCodexCachedVisibleModels_DefaultPathIsTheCLICache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "models_cache.json"),
		[]byte(`{"models":[{"slug":"gpt-6-luna","visibility":"list"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexCachedVisibleModels(""); len(got) != 1 || got[0] != "gpt-6-luna" {
		t.Fatalf("got %v, want [gpt-6-luna]", got)
	}
}

// A cache over the cap adds nothing, rather than a list parsed from part
// of the file.
func TestCodexCachedVisibleModels_OversizedCacheAddsNothing(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "models_cache.json")
	pad := strings.Repeat(" ", codexModelsCacheMaxBytes)
	if err := os.WriteFile(cache, []byte(`{"models":[{"slug":"gpt-6-luna","visibility":"list"}]}`+pad), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexCachedVisibleModels(cache); got != nil {
		t.Fatalf("oversized cache gave %v, want nil", got)
	}
}
