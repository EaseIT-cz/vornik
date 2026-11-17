package chat

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// codexModelsCacheMaxBytes caps the read of the Codex CLI's model cache;
// the file on the reference host was 177,685 bytes on 2026-11-14.
const codexModelsCacheMaxBytes = 1 << 20

// codexModelRefusalHint explains a Codex refusal that names an unsupported
// model (#772, subscription-auth.md Amendment 2026-11-14 (b)). For any
// other error body it returns "". It reads the Codex CLI's model cache
// (cachePath, or ~/.codex/models_cache.json when empty) and adds the
// models the CLI lists as visible. A missing or malformed cache adds
// nothing. The request path never reads the cache: only this error path
// and ListModels do.
func codexModelRefusalHint(body []byte, cachePath string) string {
	if !bytes.Contains(body, []byte("model_not_found")) {
		return ""
	}
	hint := "; set chat.router.codex_subscription.model to a model this account offers"
	if models := codexCachedVisibleModels(cachePath); len(models) > 0 {
		hint += " (the Codex CLI lists " + strings.Join(models, ", ") + ")"
	}
	return hint
}

// codexCachedVisibleModels returns the slugs the Codex CLI's model cache
// marks visibility "list", in file order, each once, or nil.
func codexCachedVisibleModels(cachePath string) []string {
	if cachePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		cachePath = filepath.Join(home, ".codex", "models_cache.json")
	}
	info, err := os.Stat(cachePath)
	if err != nil || info.Size() > codexModelsCacheMaxBytes {
		return nil
	}
	data, err := os.ReadFile(cachePath) //nolint:gosec // operator's own Codex CLI cache
	if err != nil {
		return nil
	}
	var cache struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if json.Unmarshal(data, &cache) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range cache.Models {
		if m.Visibility == "list" && m.Slug != "" && !seen[m.Slug] {
			seen[m.Slug] = true
			out = append(out, m.Slug)
		}
	}
	return out
}
