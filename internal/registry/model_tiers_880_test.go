package registry

// GitHub #880, project and swarm registry design §11: a shipped role names a
// modelTier; the instance maps tiers (and "<swarm>/<role>" keys) to models;
// ResolveRoleModels fills Model and ModelFallback once, at load, recording
// where each came from, and a resolved model is never written back as a pin.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
)

func tierSwarm() *Swarm {
	return &Swarm{ID: "dev-swarm", Roles: []SwarmRole{
		{Name: "pinned", Model: "own-model"},                             // 1: explicit, no fallback
		{Name: "pinned-fb", Model: "own-model", ModelFallback: "own-fb"}, // 1: explicit with fallback
		{Name: "strategist", ModelTier: "complex"},                       // 2: role key beats tier
		{Name: "lead", ModelTier: "complex"},                             // 3: tier
		{Name: "gate", ModelTier: "trivial"},                             // 3: tier with no fallback
		{Name: "scout", ModelTier: "vision"},                             // 4: unmapped tier
		{Name: "plain"},                                                  // 4: no tier
	}}
}

var testTiers = map[string]config.ModelChoice{
	"complex": {Model: "big", Fallback: "big-fb"},
	"trivial": {Model: "small"},
}

var testRoles = map[string]config.ModelChoice{
	"dev-swarm/strategist": {Model: "not-glm"},
}

func TestResolveRoleModels_880_Order(t *testing.T) {
	sw := tierSwarm()
	ResolveRoleModels(map[string]*Swarm{sw.ID: sw}, testTiers, testRoles)
	want := map[string][3]string{ // model, fallback, source
		"pinned":     {"own-model", "", "explicit"},
		"pinned-fb":  {"own-model", "own-fb", "explicit"},
		"strategist": {"not-glm", "", "role"},
		"lead":       {"big", "big-fb", "tier:complex"},
		"gate":       {"small", "", "tier:trivial"},
		"scout":      {"", "", ""},
		"plain":      {"", "", ""},
	}
	for _, r := range sw.Roles {
		got := [3]string{r.Model, r.ModelFallback, r.ModelSource}
		if got != want[r.Name] {
			t.Errorf("%s: got %q, want %q", r.Name, got, want[r.Name])
		}
	}
}

// Resolving twice (a reload re-applies the maps to fresh swarms, but a caller
// may also resolve an already-resolved swarm) must not turn a tier fill into
// an explicit pin.
func TestResolveRoleModels_880_Idempotent(t *testing.T) {
	sw := tierSwarm()
	m := map[string]*Swarm{sw.ID: sw}
	ResolveRoleModels(m, testTiers, testRoles)
	ResolveRoleModels(m, map[string]config.ModelChoice{"complex": {Model: "other"}}, nil)
	for _, r := range sw.Roles {
		if r.Name == "lead" && (r.Model != "other" || r.ModelFallback != "" || r.ModelSource != "tier:complex") {
			t.Errorf("lead re-resolved to %q/%q/%q, want other//tier:complex", r.Model, r.ModelFallback, r.ModelSource)
		}
		// Its role key is gone, so it falls through to its own tier.
		if r.Name == "strategist" && (r.Model != "other" || r.ModelSource != "tier:complex") {
			t.Errorf("strategist = %q/%q after its role key was removed, want other/tier:complex", r.Model, r.ModelSource)
		}
	}
}

// Agent namespaces keep their own rules: no tier fill, no fallback.
func TestResolveRoleModels_880_SkipsAgentNamespaces(t *testing.T) {
	id := "claudecode--helpers"
	sw := &Swarm{ID: id, Roles: []SwarmRole{{Name: "lead", ModelTier: "complex"}}}
	ResolveRoleModels(map[string]*Swarm{id: sw}, testTiers, nil)
	if r := sw.Roles[0]; r.Model != "" || r.ModelFallback != "" {
		t.Fatalf("agent swarm role resolved to %q/%q; agent namespaces are not resolved", r.Model, r.ModelFallback)
	}
}

func TestMarshalSwarm_880_DropsResolvedModels(t *testing.T) {
	sw := tierSwarm()
	for i := range sw.Roles {
		sw.Roles[i].Runtime.Image = "img"
	}
	ResolveRoleModels(map[string]*Swarm{sw.ID: sw}, testTiers, testRoles)
	out, err := MarshalSwarmMarkdown(sw)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, leaked := range []string{"big", "small", "not-glm"} {
		if strings.Contains(text, "model: "+leaked) || strings.Contains(text, "modelFallback: "+leaked) {
			t.Errorf("a resolved model %q was written back as a pin:\n%s", leaked, text)
		}
	}
	if !strings.Contains(text, "model: own-model") || !strings.Contains(text, "modelFallback: own-fb") {
		t.Errorf("an explicit model was dropped:\n%s", text)
	}
	if !strings.Contains(text, "modelTier: complex") {
		t.Errorf("modelTier not written:\n%s", text)
	}
}

func TestSwarmValidation_880_ModelTier(t *testing.T) {
	for tier, ok := range map[string]bool{
		"": true, "trivial": true, "standard": true, "complex": true, "coding": true, "vision": true,
		"huge": false, "Complex": false,
	} {
		sw := &Swarm{ID: "s", Roles: []SwarmRole{{Name: "lead", ModelTier: tier, Runtime: SwarmRoleRuntime{Image: "img"}}}}
		err := sw.Validate("s.md")
		if (err == nil) != ok {
			t.Errorf("modelTier %q: err=%v, want ok=%v", tier, err, ok)
		}
		if !ok && err != nil && !strings.Contains(err.Error(), "modelTier") {
			t.Errorf("modelTier %q: error %v does not name the field", tier, err)
		}
	}
}

// Through the loader: a tree's swarms come back resolved from the registry's
// maps, and a reload re-applies them.
func TestRegistryLoad_880_ResolvesTiers(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "swarms"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := "---\nswarmId: dev-swarm\ncoordinatorRole: lead\nroles:\n  - name: lead\n    count: 1\n    runtimePolicy: ephemeral\n    modelTier: complex\n    runtime:\n      image: img\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "swarms", "dev-swarm.md"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New()
	r.SetModelResolution(testTiers, nil)
	if err := r.LoadFromPaths(dir); err != nil {
		t.Fatal(err)
	}
	sw := r.GetSwarm("dev-swarm")
	if sw == nil || sw.Roles[0].Model != "big" || sw.Roles[0].ModelFallback != "big-fb" {
		t.Fatalf("loaded lead = %+v, want big/big-fb", sw)
	}
}

// Review 20261122-8172 F1/F3: the plain Load and a re-Stage of the same tree
// resolve alike, so the staged-vs-active diff reports no churn.
func TestRegistryStage_880_ResolvesAndDiffIsStable(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "swarms"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := "---\nswarmId: dev-swarm\ncoordinatorRole: lead\nroles:\n  - name: lead\n    count: 1\n    runtimePolicy: ephemeral\n    modelTier: complex\n    runtime:\n      image: img\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "swarms", "dev-swarm.md"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New()
	r.SetModelResolution(testTiers, nil)
	if err := r.Load(dir); err != nil {
		t.Fatal(err)
	}
	if got := r.GetSwarm("dev-swarm").Roles[0]; got.Model != "big" || got.ModelSource != "tier:complex" {
		t.Fatalf("Load: lead = %q/%q, want big/tier:complex", got.Model, got.ModelSource)
	}
	if err := r.Stage(dir); err != nil {
		t.Fatal(err)
	}
	d, err := r.DiffStaged()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ChangedSwarms) != 0 {
		t.Fatalf("re-staging the same tree reports changed swarms %v", d.ChangedSwarms)
	}
}

// Review 20261122-8172 F6: an installed map is a copy, so a caller editing
// its own map afterwards cannot change what the registry resolves with.
func TestSetModelResolution_880_Copies(t *testing.T) {
	mine := map[string]config.ModelChoice{"complex": {Model: "big"}}
	r := New()
	r.SetModelResolution(mine, nil)
	mine["complex"] = config.ModelChoice{Model: "changed"}
	if tiers, _ := r.modelResolution(); tiers["complex"].Model != "big" {
		t.Fatalf("registry sees %q after the caller edited its map", tiers["complex"].Model)
	}
}

// Review 20261122-8172 F4: a typo in a tier key is reported, not ignored.
func TestUnknownModelTiers_880(t *testing.T) {
	got := UnknownModelTiers(map[string]config.ModelChoice{"complex": {}, "coplex": {}, "vision": {}, "Coding": {}})
	if strings.Join(got, ",") != "Coding,coplex" {
		t.Fatalf("UnknownModelTiers = %v, want [Coding coplex]", got)
	}
}
