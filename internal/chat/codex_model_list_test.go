package chat

// #773 (2026-11-14): the subscription's model list was pricing.yaml's
// OpenAI rows, so `vornikctl models list` offered gpt-5.4 and
// gpt-5.4-mini, which the subscription refuses for a ChatGPT account,
// and hid a Codex model with no pricing row. With the Codex CLI's
// cache present, the list follows it (subscription-auth.md, Amendment
// 2026-11-14 (c)).

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func codexListClient(t *testing.T, cachePath string) *CodexSubscriptionClient {
	t.Helper()
	c := NewCodexSubscriptionClient("", WithCodexSubscriptionModelCatalog([]ModelInfo{
		{ID: "gpt-5.4", Source: "pricing", OwnedBy: "openai"},
		{ID: "gpt-6-luna", Source: "pricing", OwnedBy: "openai"},
	}))
	c.modelsCachePath = cachePath
	return c
}

func TestCodexSubscriptionListModels_FollowsTheCLICache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "models_cache.json")
	if err := os.WriteFile(cache, []byte(`{"models":[
		{"slug":"gpt-6-luna","visibility":"list"},
		{"slug":"gpt-5.6-new","visibility":"list"},
		{"slug":"codex-auto-review","visibility":"hide"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := codexListClient(t, cache).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelInfo{
		{ID: "gpt-6-luna", Source: "pricing", OwnedBy: "openai"},
		{ID: "gpt-5.6-new", Source: "codex-cli", OwnedBy: "openai"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestCodexSubscriptionListModels_NoCacheKeepsTheCatalog(t *testing.T) {
	got, err := codexListClient(t, filepath.Join(t.TempDir(), "absent.json")).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "gpt-5.4" || got[1].ID != "gpt-6-luna" {
		t.Fatalf("no cache changed the catalog: %+v", got)
	}
}

func TestCodexSubscriptionWithModel_KeepsCatalogAndCachePath(t *testing.T) {
	c := codexListClient(t, "/some/cache.json")
	clone, ok := c.WithModel("gpt-6-luna").(*CodexSubscriptionClient)
	if !ok {
		t.Fatal("WithModel did not return a CodexSubscriptionClient")
	}
	if clone.modelsCachePath != "/some/cache.json" || len(clone.modelCatalog) != 2 {
		t.Fatalf("clone lost fields: cache=%q catalog=%d", clone.modelsCachePath, len(clone.modelCatalog))
	}
}

func TestCodexSubscriptionListModels_NoVisibleModelKeepsTheCatalog(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "models_cache.json")
	if err := os.WriteFile(cache, []byte(`{"models":[{"slug":"codex-auto-review","visibility":"hide"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := codexListClient(t, cache).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "gpt-5.4" || got[1].ID != "gpt-6-luna" {
		t.Fatalf("a cache with no visible model changed the catalog: %+v", got)
	}
}

// A slug the cache repeats is listed once (code review review-20261114-6847).
func TestCodexSubscriptionListModels_RepeatedSlugListedOnce(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "models_cache.json")
	if err := os.WriteFile(cache, []byte(`{"models":[
		{"slug":"gpt-5.6-new","visibility":"list"},
		{"slug":"gpt-5.6-new","visibility":"list"},
		{"slug":"gpt-6-luna","visibility":"list"},
		{"slug":"gpt-6-luna","visibility":"list"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := codexListClient(t, cache).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "gpt-6-luna" || got[1].ID != "gpt-5.6-new" {
		t.Fatalf("got %+v, want gpt-6-luna then gpt-5.6-new, once each", got)
	}
}
