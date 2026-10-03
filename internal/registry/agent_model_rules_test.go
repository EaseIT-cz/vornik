package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Agent-administered design §18.6 item 2 in detail: at load, an agent
// namespace's role may not carry a modelFallback (review d94f F6, Change 7:
// a fallback firing at run time would reach a provider nobody approved), and
// its model must be in the operator's catalogue (round 2 F4: a hand edit
// fails at reload, not when a task runs).

func agentSwarmWithRole(extra string) string {
	return strings.Replace(agentSwarm, "    runtime:\n", extra+"    runtime:\n", 1)
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAgentRules_RoleModel(t *testing.T) {
	cases := map[string]struct {
		role    string
		catalog []string
		want    string // "" = loads
	}{
		"catalogue model loads":       {"    model: qwen3:35b\n", []string{"qwen3:35b"}, ""},
		"no model loads":              {"", []string{}, ""},
		"off-catalogue hand edit":     {"    model: gpt-5\n", []string{"qwen3:35b"}, "not in the operator's catalogue"},
		"any model, empty catalogue":  {"    model: qwen3:35b\n", []string{}, "not in the operator's catalogue"},
		"modelFallback is refused":    {"    model: qwen3:35b\n    modelFallback: qwen3:35b\n", []string{"qwen3:35b"}, "modelFallback"},
		"modelFallback alone refused": {"    modelFallback: gpt-5\n", []string{"qwen3:35b"}, "modelFallback"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			files := baseAgentTree()
			files["swarms/hermes--fin.md"] = agentSwarmWithRole(c.role)
			r := New()
			r.SetAgentModelCatalogue(c.catalog)
			if err := r.Stage(writeTree(t, files)); err != nil {
				t.Fatal(err)
			}
			rs := r.staged.index.Rejected
			if c.want == "" {
				if len(rs) != 0 {
					t.Fatalf("rejected: %+v", rs)
				}
				return
			}
			if !hasRejection(rs, c.want) {
				t.Fatalf("not rejected with %q: %+v", c.want, rs)
			}
			if _, ok := r.staged.swarms["hermes--fin"]; ok {
				t.Fatal("the rejected swarm stays in the set")
			}
		})
	}
}

// An operator swarm is not an agent's: its models and fallbacks are the
// operator's own configuration (round 3 F6).
func TestAgentRules_OperatorRoleModelUntouched(t *testing.T) {
	files := map[string]string{"swarms/ops.md": strings.Replace(agentSwarmWithRole("    model: anything\n    modelFallback: other\n"), "hermes--fin", "ops", 1)}
	r := New()
	r.SetAgentModelCatalogue([]string{})
	if err := r.Stage(writeTree(t, files)); err != nil {
		t.Fatal(err)
	}
	if rs := r.staged.index.Rejected; len(rs) != 0 {
		t.Fatalf("an operator swarm was judged by the agent rules: %+v", rs)
	}
}

// Review 20261003-a525 A1: a registry given no catalogue fails closed. A
// hand-edited agent role naming a model is refused when nothing set a
// catalogue, as the agentadmin side refuses any model with an empty one.
func TestAgentRules_NoCatalogueSetFailsClosed(t *testing.T) {
	files := baseAgentTree()
	files["swarms/hermes--fin.md"] = agentSwarmWithRole("    model: gpt-5\n")
	r := New()
	if err := r.Stage(writeTree(t, files)); err != nil {
		t.Fatal(err)
	}
	if rs := r.staged.index.Rejected; !hasRejection(rs, "not in the operator's catalogue") {
		t.Fatalf("an off-catalogue agent role loaded with no catalogue set: %+v", rs)
	}
	if rs := rejectedFor(t, files); !hasRejection(rs, "not in the operator's catalogue") {
		t.Fatalf("loadConfigSet with no catalogue admitted it: %+v", rs)
	}
}

// The daemon sets the process default at boot, so every registry it builds
// (the doctor's, the wizard's, the UI's) judges by the same catalogue.
func TestAgentRules_DefaultCatalogueReachesNewRegistries(t *testing.T) {
	SetDefaultAgentModelCatalogue([]string{"qwen3:35b"})
	t.Cleanup(func() { SetDefaultAgentModelCatalogue(nil) })
	files := baseAgentTree()
	files["swarms/hermes--fin.md"] = agentSwarmWithRole("    model: qwen3:35b\n")
	r := New()
	if err := r.Stage(writeTree(t, files)); err != nil {
		t.Fatal(err)
	}
	if rs := r.staged.index.Rejected; len(rs) != 0 {
		t.Fatalf("a catalogue model was refused under the default catalogue: %+v", rs)
	}
}
