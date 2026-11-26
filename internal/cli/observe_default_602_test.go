package cli

// GitHub #602 (design 11, amendment 2026-11-20, B6): observe is the default
// for new projects, so turning autonomy on is safe by construction. Every
// shipped template and example config that carries an autonomy block sets
// observe: true, and `vornikctl init project --autonomy` writes it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/templates"
)

type observeProbe struct {
	Autonomy *struct {
		Enabled bool `yaml:"enabled"`
		Observe bool `yaml:"observe"`
	} `yaml:"autonomy"`
}

func TestShippedTemplatesDefaultToObserve_602(t *testing.T) {
	configsDir := repoConfigsDir(t)
	cat, err := templates.Load(filepath.Join(configsDir, "project-templates"))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	examined := 0
	for _, slug := range allTemplateSlugs(t, configsDir) {
		params := shippedTemplateSamples[slug]
		m, _ := cat.Get(slug)
		var rendered map[string]string
		if m.NeedsMultiValue() {
			rendered, err = cat.MaterialiseFilesMulti(m, params, nil)
		} else {
			flat := map[string]string{}
			for k, v := range params {
				if len(v) > 0 {
					flat[k] = v[len(v)-1]
				}
			}
			rendered, err = cat.MaterialiseFiles(m, flat)
		}
		if err != nil {
			t.Fatalf("%s: materialise: %v", slug, err)
		}
		for name, body := range rendered {
			if !strings.HasPrefix(name, "projects/") && !strings.HasSuffix(name, "project.yaml") {
				continue
			}
			var p observeProbe
			if err := yaml.Unmarshal([]byte(body), &p); err != nil || p.Autonomy == nil {
				continue
			}
			examined++
			if !p.Autonomy.Observe {
				t.Errorf("template %s (%s): autonomy block without observe: true", slug, name)
			}
		}
	}
	t.Logf("template project files with an autonomy block examined: %d", examined)
	if examined == 0 {
		t.Fatal("no template project file examined")
	}
}

func TestExampleConfigsDefaultToObserve_602(t *testing.T) {
	configsDir := repoConfigsDir(t)
	var paths []string
	for _, pat := range []string{"examples/*.yaml", "projects/*.yaml"} {
		got, _ := filepath.Glob(filepath.Join(configsDir, pat))
		paths = append(paths, got...)
	}
	examined := 0
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var p observeProbe
		if yaml.Unmarshal(raw, &p) != nil || p.Autonomy == nil {
			continue
		}
		examined++
		if !p.Autonomy.Observe {
			t.Errorf("%s: autonomy block without observe: true", filepath.Base(path))
		}
	}
	t.Logf("example configs with an autonomy block examined: %d", examined)
	if examined == 0 {
		t.Fatal("no example config examined")
	}
}

func TestInitProjectWithAutonomyWritesObserve_602(t *testing.T) {
	// A copy of the repository's configs, so the generated project
	// validates against real swarms and workflows.
	dir := filepath.Join(t.TempDir(), "configs")
	if err := os.CopyFS(dir, os.DirFS(repoConfigsDir(t))); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "projects", "observe-proj.yaml"))
	prev := [...]any{initConfigDir, initAutonomy, initGoal, initDisplayName, initTemplate}
	t.Cleanup(func() {
		initConfigDir, initAutonomy, initGoal = prev[0].(string), prev[1].(bool), prev[2].(string)
		initDisplayName, initTemplate = prev[3].(string), prev[4].(string)
	})
	initConfigDir, initAutonomy, initGoal, initDisplayName, initTemplate = dir, true, "keep it healthy", "Observe Proj", ""
	if err := runInitProject(initProjectCmd, []string{"observe-proj"}); err != nil {
		t.Fatalf("init project: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "projects", "observe-proj.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var p observeProbe
	if err := yaml.Unmarshal(raw, &p); err != nil || p.Autonomy == nil {
		t.Fatalf("generated project has no autonomy block: %v\n%s", err, raw)
	}
	if !p.Autonomy.Enabled || !p.Autonomy.Observe {
		t.Fatalf("init project --autonomy must write enabled and observe true:\n%s", raw)
	}
}
