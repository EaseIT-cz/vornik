package api

// GitHub #880, project and swarm registry design §11.2 R7: setup/managed
// takes optional models.tiers and models.roles; every model and fallback in
// them must be a catalogue id, or the call is refused and writes nothing.
// Operations API 1.6.0.

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/opsapi"
)

func TestSetupManaged880_TiersFromTheCatalogue(t *testing.T) {
	r := newSetupRig582(t, nil)
	b := managedSetupBody582("owner-sub-1")
	models := b["models"].(map[string]any)
	models["tiers"] = map[string]any{"coding": map[string]any{"model": "open-b", "fallback": "open-a"}, "complex": map[string]any{"model": "open-a"}}
	models["roles"] = map[string]any{"trading-swarm/strategist": map[string]any{"model": "open-b"}}
	code, out := r.post(t, b)
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, true, out["changed"])

	cfg, err := config.LoadFromPath(r.path)
	require.NoError(t, err)
	require.Equal(t, config.ModelChoice{Model: "open-b", Fallback: "open-a"}, cfg.Runtime.AgentLLM.Tiers["coding"])
	require.Equal(t, config.ModelChoice{Model: "open-a"}, cfg.Runtime.AgentLLM.Tiers["complex"])
	require.Equal(t, "open-b", cfg.Runtime.AgentLLM.Roles["trading-swarm/strategist"].Model)

	// The same call again changes nothing.
	before, _ := os.ReadFile(r.path)
	code, out = r.post(t, b)
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, false, out["changed"])
	after, _ := os.ReadFile(r.path)
	require.Equal(t, string(before), string(after))

	// A call without tiers keeps them: models without tiers is not a removal.
	code, out = r.post(t, managedSetupBody582("owner-sub-1"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	cfg, err = config.LoadFromPath(r.path)
	require.NoError(t, err)
	require.Equal(t, "open-b", cfg.Runtime.AgentLLM.Tiers["coding"].Model)
}

func TestSetupManaged880_RefusesOffCatalogueOrUnknownTier(t *testing.T) {
	for name, mutate := range map[string]func(m map[string]any){
		"tier model off the catalogue": func(m map[string]any) {
			m["tiers"] = map[string]any{"complex": map[string]any{"model": "zai.glm-5"}}
		},
		"tier fallback off the catalogue": func(m map[string]any) {
			m["tiers"] = map[string]any{"complex": map[string]any{"model": "open-a", "fallback": "kimi-k2.6"}}
		},
		"role model off the catalogue": func(m map[string]any) {
			m["roles"] = map[string]any{"dev-swarm/coder": map[string]any{"model": "qwen"}}
		},
		"unknown tier": func(m map[string]any) {
			m["tiers"] = map[string]any{"coplex": map[string]any{"model": "open-a"}}
		},
		"role key with a dot": func(m map[string]any) {
			m["roles"] = map[string]any{"a.b/c": map[string]any{"model": "open-a"}}
		},
		"role key with two slashes": func(m map[string]any) {
			m["roles"] = map[string]any{"a/b/c": map[string]any{"model": "open-a"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig582(t, nil)
			before, _ := os.ReadFile(r.path)
			b := managedSetupBody582("owner-sub-1")
			mutate(b["models"].(map[string]any))
			code, out := r.post(t, b)
			require.Equal(t, http.StatusBadRequest, code, "%v", out)
			// Refused by the tier check, not as an unknown field (which is how
			// a daemon before 1.6.0 refuses any tiers at all).
			msg, _ := out["error"].(map[string]any)["message"].(string)
			require.Regexp(t, `models\.(tiers|roles)`, msg)
			require.NotContains(t, msg, "unknown field")
			after, _ := os.ReadFile(r.path)
			require.Equal(t, string(before), string(after), "a refused call must write nothing")
		})
	}
}

// Review 20261122-f58e F1: each map is replaced only when sent. A call that
// sends tiers and no roles keeps the roles an earlier call wrote, and the
// other way round.
func TestSetupManaged880_EachMapKeptWhenLeftOut(t *testing.T) {
	r := newSetupRig582(t, nil)
	b := managedSetupBody582("owner-sub-1")
	models := b["models"].(map[string]any)
	models["tiers"] = map[string]any{"complex": map[string]any{"model": "open-a"}}
	models["roles"] = map[string]any{"trading-swarm/strategist": map[string]any{"model": "open-b"}}
	code, out := r.post(t, b)
	require.Equal(t, http.StatusOK, code, "%v", out)

	b = managedSetupBody582("owner-sub-1")
	b["models"].(map[string]any)["tiers"] = map[string]any{"coding": map[string]any{"model": "open-b"}}
	code, out = r.post(t, b)
	require.Equal(t, http.StatusOK, code, "%v", out)
	cfg, err := config.LoadFromPath(r.path)
	require.NoError(t, err)
	require.Equal(t, map[string]config.ModelChoice{"coding": {Model: "open-b"}}, cfg.Runtime.AgentLLM.Tiers, "tiers sent: replaced whole")
	require.Equal(t, "open-b", cfg.Runtime.AgentLLM.Roles["trading-swarm/strategist"].Model, "roles left out: kept")

	b = managedSetupBody582("owner-sub-1")
	b["models"].(map[string]any)["roles"] = map[string]any{"dev-swarm/coder": map[string]any{"model": "open-a"}}
	code, out = r.post(t, b)
	require.Equal(t, http.StatusOK, code, "%v", out)
	cfg, err = config.LoadFromPath(r.path)
	require.NoError(t, err)
	require.Equal(t, "open-b", cfg.Runtime.AgentLLM.Tiers["coding"].Model, "tiers left out: kept")
	require.Equal(t, map[string]config.ModelChoice{"dev-swarm/coder": {Model: "open-a"}}, cfg.Runtime.AgentLLM.Roles, "roles sent: replaced whole")
}

// Review 20261122-085b: an empty map sent clears that map; a call sending
// neither map leaves both.
func TestSetupManaged880_EmptyMapClears(t *testing.T) {
	r := newSetupRig582(t, nil)
	b := managedSetupBody582("owner-sub-1")
	b["models"].(map[string]any)["tiers"] = map[string]any{"complex": map[string]any{"model": "open-a"}}
	b["models"].(map[string]any)["roles"] = map[string]any{"dev-swarm/coder": map[string]any{"model": "open-b"}}
	code, out := r.post(t, b)
	require.Equal(t, http.StatusOK, code, "%v", out)

	code, out = r.post(t, managedSetupBody582("owner-sub-1"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, false, out["changed"], "neither map sent: nothing changes")

	b = managedSetupBody582("owner-sub-1")
	b["models"].(map[string]any)["tiers"] = map[string]any{}
	code, out = r.post(t, b)
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, true, out["changed"], "an empty tiers map is a change: it clears them")
	cfg, err := config.LoadFromPath(r.path)
	require.NoError(t, err)
	require.Empty(t, cfg.Runtime.AgentLLM.Tiers)
	require.Equal(t, "open-b", cfg.Runtime.AgentLLM.Roles["dev-swarm/coder"].Model)

	// And the same for roles (review 20261122-46fe finding 1).
	b = managedSetupBody582("owner-sub-1")
	b["models"].(map[string]any)["tiers"] = map[string]any{"complex": map[string]any{"model": "open-a"}}
	b["models"].(map[string]any)["roles"] = map[string]any{}
	code, out = r.post(t, b)
	require.Equal(t, http.StatusOK, code, "%v", out)
	cfg, err = config.LoadFromPath(r.path)
	require.NoError(t, err)
	require.Empty(t, cfg.Runtime.AgentLLM.Roles)
	require.Equal(t, "open-a", cfg.Runtime.AgentLLM.Tiers["complex"].Model)
}

// The published contract says what the handler does: 1.6.0, with tiers and
// roles in setup/managed's models block.
func TestOpsContract880_ManagedTiers(t *testing.T) {
	raw, ok := opsapi.Document("v1")
	if !ok {
		t.Fatal("no v1 document")
	}
	doc := string(raw)
	if !strings.Contains(doc, "  version: 1.6.0") {
		t.Fatal("the contract is not 1.6.0")
	}
	_, rest, _ := strings.Cut(doc, "catalogue:")
	block, _, _ := strings.Cut(rest, "templates:")
	for _, f := range []string{"tiers:", "roles:", "Since 1.6.0", "#/components/schemas/ModelChoice"} {
		if !strings.Contains(block, f) {
			t.Errorf("setup/managed models block lacks %q", f)
		}
	}
}
