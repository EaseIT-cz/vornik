package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Review 20261003-a525 A1: the registry fails closed with no catalogue, so a
// CLI command that loads the deployed tree reads agent_admin.models from the
// config.yaml beside it; without one, an agent role naming a model is
// refused there too.
func TestNewCLIRegistry_ReadsTheCatalogueBesideTheTree(t *testing.T) {
	t.Setenv("VORNIK_CONFIG", "")
	root := t.TempDir()
	configs := filepath.Join(root, "configs")
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(configs, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	swarm := "---\nswarmId: hermes--fin\nleadRole: worker\nroles:\n  - name: worker\n    model: qwen3:35b\n    runtime:\n      image: img\n    permissions:\n      allowedTools: [file_read]\n---\n"
	if err := os.WriteFile(filepath.Join(configs, "swarms", "hermes--fin.md"), []byte(swarm), 0o644); err != nil {
		t.Fatal(err)
	}
	rejected := func() bool {
		reg := newCLIRegistry(configs)
		_ = reg.Load(configs)
		return reg.GetSwarm("hermes--fin") == nil
	}
	if !rejected() {
		t.Fatal("with no config.yaml an agent role naming a model loaded")
	}
	cfg := "agent_admin:\n  models:\n    - id: qwen3:35b\n      good_for: drafting\n"
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if rejected() {
		t.Fatal("a catalogue model was refused with the catalogue in config.yaml")
	}
}
