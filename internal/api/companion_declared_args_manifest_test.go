package api

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// declaredArgsManifest is the companion tools' declared top-level arguments,
// exported for the Python clients' fake daemon
// (contrib/hermes-companion/tests/helpers.py), so the Hermes plugin's tests
// refuse what the daemon refuses (LLD 21 §A amendment 2026-10-14, GitHub #305
// follow-up). Until then every client fake in the repo accepted any argument,
// which is how the memory benchmark's recall "max_tokens" passed its tests
// and failed the 2026.10.6 release benchmark.
const declaredArgsManifest = "testdata/companion_declared_args.json"

func buildDeclaredArgsManifest() []byte {
	tools := make([]string, 0, len(companionToolArgs))
	for tool := range companionToolArgs {
		tools = append(tools, tool)
	}
	sort.Strings(tools)
	out := map[string][]string{}
	for _, tool := range tools {
		fields := jsonFieldNames(companionToolArgs[tool]())
		if fields == nil {
			fields = []string{}
		}
		out[tool] = fields
	}
	b, _ := json.MarshalIndent(map[string]any{
		"_comment": "GENERATED from internal/api/companion_strict_args.go companionToolArgs by " +
			"TestCompanionDeclaredArgsManifest; regenerate with UPDATE_DECLARED_ARGS=1 go test ./internal/api " +
			"-run TestCompanionDeclaredArgsManifest. Read by contrib/hermes-companion/tests/helpers.py.",
		"tools": out,
	}, "", "  ")
	return append(b, '\n')
}

// The manifest is the registry, byte for byte: a stale manifest would let the
// Python fake accept what the daemon refuses.
func TestCompanionDeclaredArgsManifest(t *testing.T) {
	want := buildDeclaredArgsManifest()
	if os.Getenv("UPDATE_DECLARED_ARGS") == "1" {
		require.NoError(t, os.WriteFile(declaredArgsManifest, want, 0o644))
	}
	got, err := os.ReadFile(declaredArgsManifest)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got),
		"%s is stale: regenerate with UPDATE_DECLARED_ARGS=1 go test ./internal/api -run TestCompanionDeclaredArgsManifest", declaredArgsManifest)
}
