package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Router.Resolves is the doctor's view of routing (model-route-coverage
// design, 2026-09-24). It must be the SAME decision dispatch makes: the
// doctor's old copy of the rule treated an empty-prefix suffix route as a
// catch-all and so passed every model whenever OpenRouter was configured.
func TestRouter_ResolvesAgreesWithDispatch(t *testing.T) {
	openrouter := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "openrouter"}}
	codex := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "codex-subscription"}}
	bedrock := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "bedrock"}}
	r, err := NewRouter(bedrock, []Route{
		{Suffix: ":free", Provider: openrouter, Name: "openrouter"},
		{Prefix: "gpt-", Provider: codex, Name: "codex-subscription"},
	}, WithRouterFallbackName("bedrock"))
	require.NoError(t, err)

	cases := []struct {
		model   string
		route   string
		matched bool
	}{
		{"deepseek/deepseek-r1:free", "openrouter", true},
		{"gpt-5.4", "codex-subscription", true},
		{"gpt-5.4:free", "openrouter", true}, // suffix pass precedes prefix pass
		{"mistral.large", "fallback", false}, // the suffix route is NOT a catch-all
		{"", "fallback", false},
	}
	for _, c := range cases {
		route, matched := r.Resolves(c.model)
		assert.Equal(t, c.route, route, c.model)
		assert.Equal(t, c.matched, matched, c.model)
		if c.model != "" {
			want := c.route
			if !c.matched {
				want = "bedrock"
			}
			assert.Equal(t, want, dispatchModel(t, r, c.model), "Resolves disagrees with dispatch for %q", c.model)
		}
	}
}

func TestRouter_ResolvesAnEmptyPrefixNoSuffixRouteAsCatchAll(t *testing.T) {
	catch := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "http"}}
	fallback := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "bedrock"}}
	r, err := NewRouter(fallback, []Route{{Prefix: "", Provider: catch, Name: "http"}})
	require.NoError(t, err)
	route, matched := r.Resolves("anything")
	assert.Equal(t, "http", route)
	assert.True(t, matched)
}
