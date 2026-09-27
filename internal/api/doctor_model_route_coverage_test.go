package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/pricing"
)

// prefixResolver stands in for Router.Resolves in the pure tests: every
// listed prefix is an explicit route named after itself.
func prefixResolver(prefixes ...string) func(string) (string, bool) {
	for _, p := range prefixes {
		if p == "" {
			// An empty prefix would reproduce the old catch-all bug in the
			// test double itself; route such cases through a real chat.Router.
			panic("prefixResolver: empty prefix — use chat.NewRouter(...).Resolves")
		}
	}
	return func(model string) (string, bool) {
		for _, p := range prefixes {
			if strings.HasPrefix(model, p) {
				return p, true
			}
		}
		return "fallback", false
	}
}

// loadPricingFromString writes the YAML to a temp file and loads it, so the
// route-coverage tests can build small pricing tables without touching the
// real configs/pricing.yaml.
func loadPricingFromString(t *testing.T, yaml string) *pricing.Table {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pricing.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write pricing: %v", err)
	}
	table, err := pricing.Load(p)
	if err != nil {
		t.Fatalf("load pricing: %v", err)
	}
	return table
}

// TestModelRouteCoverage_Pure_FullyCovered: a model with both a matching
// route prefix and a pricing entry produces no findings.
func TestModelRouteCoverage_Pure_FullyCovered(t *testing.T) {
	table := loadPricingFromString(t, "models:\n  zai.glm-5:\n    input: 1\n    output: 2\n")
	refs := []modelRef{{model: "zai.glm-5", swarm: "s", role: "reviewer"}}
	findings := evalModelRouteCoverage(refs, prefixResolver("zai."), table)
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", findings)
	}
}

// TestModelRouteCoverage_Pure_Unrouted: a model that matches no route prefix
// is flagged as unrouted.
func TestModelRouteCoverage_Pure_Unrouted(t *testing.T) {
	table := loadPricingFromString(t, "models:\n  mistral.large:\n    input: 1\n    output: 2\n")
	refs := []modelRef{{model: "mistral.large", swarm: "s", role: "coder"}}
	findings := evalModelRouteCoverage(refs, prefixResolver("zai."), table)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %v", findings)
	}
	if !strings.Contains(findings[0], "unrouted") {
		t.Errorf("finding should call out unrouted; got %q", findings[0])
	}
}

// TestModelRouteCoverage_Pure_Unpriced: a routed model with no pricing entry
// is flagged as unpriced.
func TestModelRouteCoverage_Pure_Unpriced(t *testing.T) {
	table := pricing.Empty()
	refs := []modelRef{{model: "zai.glm-5", swarm: "s", role: "reviewer"}}
	findings := evalModelRouteCoverage(refs, prefixResolver("zai."), table)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %v", findings)
	}
	if !strings.Contains(findings[0], "unpriced") {
		t.Errorf("finding should call out unpriced; got %q", findings[0])
	}
}

// TestModelRouteCoverage_Pure_FallbackChecked: modelFallback is covered too —
// a fully-routed/priced primary with an unrouted fallback still flags.
func TestModelRouteCoverage_Pure_FallbackChecked(t *testing.T) {
	table := loadPricingFromString(t, "models:\n  zai.glm-5:\n    input: 1\n    output: 2\n  bad.fallback:\n    input: 1\n    output: 2\n")
	refs := []modelRef{
		{model: "zai.glm-5", swarm: "s", role: "reviewer"},
		{model: "bad.fallback", swarm: "s", role: "reviewer", isFallback: true},
	}
	findings := evalModelRouteCoverage(refs, prefixResolver("zai."), table)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for the fallback, got %v", findings)
	}
	if !strings.Contains(findings[0], "bad.fallback") || !strings.Contains(findings[0], "fallback") {
		t.Errorf("finding should name the fallback model; got %q", findings[0])
	}
}

// TestCheckModelRouteCoverage_Integration exercises the full check end-to-end
// from a temp config dir + pricing file + injected route prefixes.
func TestCheckModelRouteCoverage_Integration(t *testing.T) {
	dir := t.TempDir()
	swarmsDir := filepath.Join(dir, "swarms")
	if err := os.MkdirAll(swarmsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	swarm := "---\n" +
		"swarmId: \"s\"\n" +
		"roles:\n" +
		"  - name: \"reviewer\"\n" +
		"    model: \"zai.glm-5\"\n" +
		"    modelFallback: \"orphan.model\"\n" +
		"    runtime:\n" +
		"      image: \"vornik-agent:latest\"\n" +
		"---\n"
	if err := os.WriteFile(filepath.Join(swarmsDir, "s.md"), []byte(swarm), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}
	pricingPath := filepath.Join(dir, "pricing.yaml")
	if err := os.WriteFile(pricingPath, []byte("models:\n  zai.glm-5:\n    input: 1\n    output: 2\n"), 0o644); err != nil {
		t.Fatalf("write pricing: %v", err)
	}

	h := &DoctorHandlers{
		configDir:         dir,
		pricingPath:       pricingPath,
		chatRouteResolver: prefixResolver("zai."),
	}
	got := h.checkModelRouteCoverage()
	if got.Status != "WARNING" {
		t.Fatalf("status = %q, want WARNING; msg=%q items=%v", got.Status, got.Message, got.Items)
	}
	// orphan.model is both unrouted and unpriced → one finding naming it.
	joined := strings.Join(got.Items, "\n")
	if !strings.Contains(joined, "orphan.model") {
		t.Errorf("orphan.model should be flagged; items=%v", got.Items)
	}
	if strings.Contains(joined, "zai.glm-5") {
		t.Errorf("covered primary should not be flagged; items=%v", got.Items)
	}
}

// TestCheckModelRouteCoverage_NoRoutesSkips: with no router resolver wired the
// check skips (can't meaningfully assert coverage) — and says why.
func TestCheckModelRouteCoverage_NoRoutesSkips(t *testing.T) {
	h := &DoctorHandlers{configDir: "testdata"}
	got := h.checkModelRouteCoverage()
	if got.Status != "SKIPPED" {
		t.Errorf("status = %q, want SKIPPED; msg=%q", got.Status, got.Message)
	}
}

// Issue #61(b): gpt-5.4 is served by codex-subscription through a DEFAULT route
// the router appends because that sub-provider is enabled; the operator's
// routes list never names "gpt-". Asked of the real router, it is routed.
func TestModelRouteCoverage_AsksTheRouterNotTheOperatorRoutes(t *testing.T) {
	codex := &resolveStub{}
	bedrock := &resolveStub{}
	r, err := chat.NewRouter(bedrock, []chat.Route{
		{Prefix: "zai.", Provider: bedrock, Name: "bedrock"},          // what the operator wrote
		{Prefix: "gpt-", Provider: codex, Name: "codex-subscription"}, // merged-in default
	})
	if err != nil {
		t.Fatal(err)
	}
	table := loadPricingFromString(t, "models:\n  gpt-5.4:\n    input: 1\n    output: 2\n")
	refs := []modelRef{{model: "gpt-5.4", swarm: "assistant-swarm", role: "lead", isFallback: true}}
	if f := evalModelRouteCoverage(refs, r.Resolves, table); len(f) != 0 {
		t.Fatalf("a model the router serves was reported: %v", f)
	}
}

// The false pass: an empty-prefix SUFFIX route is not a catch-all.
func TestModelRouteCoverage_ASuffixRouteDoesNotCoverEverything(t *testing.T) {
	or := &resolveStub{}
	r, err := chat.NewRouter(&resolveStub{}, []chat.Route{{Suffix: ":free", Provider: or, Name: "openrouter"}})
	if err != nil {
		t.Fatal(err)
	}
	table := loadPricingFromString(t, "models:\n  mistral.large:\n    input: 1\n    output: 2\n")
	f := evalModelRouteCoverage([]modelRef{{model: "mistral.large", swarm: "s", role: "coder"}}, r.Resolves, table)
	if len(f) != 1 || !strings.Contains(f[0], "unrouted") {
		t.Fatalf("a model no route matches passed as routed: %v", f)
	}
}

func TestModelRouteCoverage_AnUnpricedFindingNamesTheRoute(t *testing.T) {
	f := evalModelRouteCoverage([]modelRef{{model: "zai.glm-9", swarm: "s", role: "r"}},
		prefixResolver("zai."), pricing.Empty())
	if len(f) != 1 || !strings.Contains(f[0], `route "zai."`) {
		t.Fatalf("the finding does not say which route would bill it: %v", f)
	}
}

func TestCheckModelRouteCoverage_NoResolverSaysTheRouterIsNotInUse(t *testing.T) {
	got := (&DoctorHandlers{configDir: "testdata"}).checkModelRouteCoverage()
	if got.Status != "SKIPPED" || !strings.Contains(got.Message, "router") {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
}

// resolveStub is the smallest chat.Provider a Router accepts; Resolves never
// calls it.
type resolveStub struct{ chat.Provider }

// Review 609e F5: the whole doctor path — checkModelRouteCoverage, not only
// the pure evaluator — with a REAL router's Resolves, the seam the container
// wires. Issue #61(b)'s model must come out clean.
func TestCheckModelRouteCoverage_EndToEndWithARealRouter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "swarms"), 0o755); err != nil {
		t.Fatal(err)
	}
	swarm := "---\nswarmId: \"assistant-swarm\"\nroles:\n  - name: \"lead\"\n    model: \"zai.glm-5\"\n" +
		"    modelFallback: \"gpt-5.4\"\n    runtime:\n      image: \"vornik-agent:latest\"\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "swarms", "assistant-swarm.md"), []byte(swarm), 0o644); err != nil {
		t.Fatal(err)
	}
	pricingPath := filepath.Join(dir, "pricing.yaml")
	if err := os.WriteFile(pricingPath, []byte("models:\n  zai.glm-5:\n    input: 1\n    output: 2\n  gpt-5.4:\n    input: 1\n    output: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := chat.NewRouter(&resolveStub{}, []chat.Route{
		{Prefix: "zai.", Provider: &resolveStub{}, Name: "bedrock"},
		{Prefix: "gpt-", Provider: &resolveStub{}, Name: "codex-subscription"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &DoctorHandlers{configDir: dir, pricingPath: pricingPath}
	h.SetChatRouteResolver(r.Resolves)
	got := h.checkModelRouteCoverage()
	if got.Status != "OK" {
		t.Fatalf("status %s %q items=%v", got.Status, got.Message, got.Items)
	}
}
