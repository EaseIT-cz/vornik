package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

// GitHub #683 (loader-validator agreement design §12, §15): the production
// reload path (config.ConfigReloader, wired in initRegistry) ran Stage, the
// strip and ActivateStaged with no rejection gate, so a project file that
// stopped decoding was dropped from a running daemon on hot reload.

func tree683(t *testing.T, keeper string) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"workflows/w1.md":      "---\nworkflowId: \"w1\"\ndisplayName: \"W\"\nentrypoint: \"go\"\nsteps:\n  go:\n    type: \"agent\"\n    role: \"coder\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n\n## Prompts\n\n### go\n\nDo the thing.\n",
		"swarms/s1.md":         "---\nswarmId: \"s1\"\ndisplayName: \"S\"\nroles:\n  - name: \"coder\"\n    runtime:\n      image: \"test:latest\"\n---\n",
		"projects/keeper.yaml": keeper,
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const keeper683 = "projectId: \"keeper\"\ndisplayName: \"Keeper\"\nswarmId: \"s1\"\ndefaultWorkflowId: \"w1\"\n"

// The service's reload steps, in the ConfigReloader's order, keep a running
// project whose file stops decoding, and report it.
func TestReloadGate683_ARefusedProjectFileIsQuarantinedNotDropped(t *testing.T) {
	dir := tree683(t, keeper683)
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatal(err)
	}
	c := &Container{Logger: zerolog.Nop(), Registry: reg}
	if err := os.WriteFile(filepath.Join(dir, "projects", "keeper.yaml"),
		[]byte(keeper683+"autonomy:\n  feeds_from_a_newer_release: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reloader := config.NewConfigReloader(nil, zerolog.Nop())
	// loader, validator and activator: the calls initRegistry makes.
	if err := reg.Stage(dir); err != nil {
		t.Fatal(err)
	}
	if err := c.gateRegistryReload(); err != nil {
		t.Fatalf("gate refused a project-only rejection: %v", err)
	}
	_ = reg.StripInvalidFromStaged()
	if err := reg.CheckStagedKeepsRunning(); err != nil {
		t.Fatalf("keeper was carried, nothing is dropped: %v", err)
	}
	if err := reg.ActivateStaged(); err != nil {
		t.Fatal(err)
	}
	c.reportReloadQuarantine(reloader)
	if reg.GetProject("keeper") == nil {
		t.Fatal("the running project was dropped by the reload (the #683 defect)")
	}
	if st := reloader.Status(); !st.HasWarnings || !strings.Contains(strings.Join(st.Warnings, "\n"), "keeper") {
		t.Fatalf("the quarantine must be a reload warning naming the project: %+v", st.Warnings)
	}
}

// The law: initRegistry's reload closures call the gate, the re-check and the
// report, so ConfigReloader cannot bypass §12 again.
func TestReloadGate683_InitRegistryWiresTheGate(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "container_registry.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"gateRegistryReload": false, "CheckStagedKeepsRunning": false, "reportReloadQuarantine": false}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "initRegistry" {
			return true
		}
		ast.Inspect(fn, func(m ast.Node) bool {
			if sel, ok := m.(*ast.SelectorExpr); ok {
				if _, tracked := want[sel.Sel.Name]; tracked {
					want[sel.Sel.Name] = true
				}
			}
			return true
		})
		return false
	})
	for name, found := range want {
		if !found {
			t.Errorf("initRegistry's reload closures do not call %s (GitHub #683)", name)
		}
	}
}
