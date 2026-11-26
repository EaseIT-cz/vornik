package executor

// GitHub #881, registry design §12.2 C3: on a managed instance a step whose
// resolved model is outside the catalogue fails before its container starts,
// naming the model. The setup model (not in a role) is the default and runs.

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func TestCheckManagedCatalogue_881(t *testing.T) {
	e := &Executor{config: &Config{
		AgentLLMEnv:           map[string]string{"VORNIK_LLM_MODEL": "setup-model"},
		ManagedModelCatalogue: []string{"open-a", "setup-model"},
	}}
	role := &registry.SwarmRole{Name: "lead"}

	if err := e.checkManagedCatalogue(role, "open-a"); err != nil {
		t.Errorf("catalogue model refused: %v", err)
	}
	if err := e.checkManagedCatalogue(role, "setup-model"); err != nil {
		t.Errorf("setup model refused: %v", err)
	}
	err := e.checkManagedCatalogue(role, "zai.glm-5")
	if err == nil || !strings.Contains(err.Error(), "zai.glm-5") || !strings.Contains(err.Error(), "catalogue") {
		t.Errorf("off-catalogue model: %v", err)
	}
	e.config.ManagedModelCatalogue = nil // not managed
	if err := e.checkManagedCatalogue(role, "zai.glm-5"); err != nil {
		t.Errorf("not managed, refused: %v", err)
	}
}
