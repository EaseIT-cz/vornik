package templates_test

// GitHub #880, project and swarm registry design §11: Vornik assumes no model
// provider. No shipped swarm names a model or a fallback, no project template
// rendered with its defaults does, and no shipped project file pins the
// hallucination judge. Instead every shipped role names a modelTier, which
// the instance maps to its own models (an unmapped tier uses the setup
// model). Before the fix the shipped swarms pinned 42 models and 41
// fallbacks (Bedrock and Ollama names) and three trading files pinned the
// judge; no role named a tier.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/rolelibrary"
	"vornik.io/vornik/internal/templates"
)

const configsRoot = "../../configs"

func pinsOf(sw *registry.Swarm) []string {
	var out []string
	for _, r := range sw.Roles {
		if r.Model != "" {
			out = append(out, r.Name+": model "+r.Model)
		}
		if r.ModelFallback != "" {
			out = append(out, r.Name+": modelFallback "+r.ModelFallback)
		}
		if m := r.Runtime.EnvVars["VORNIK_LLM_MODEL"]; m != "" {
			out = append(out, r.Name+": envVars VORNIK_LLM_MODEL "+m)
		}
	}
	return out
}

func TestShippedConfigsNameNoModel_880(t *testing.T) {
	files, roles := 0, 0

	// configs/swarms/*.md
	swarms, err := filepath.Glob(filepath.Join(configsRoot, "swarms", "*.md"))
	if err != nil || len(swarms) == 0 {
		t.Fatalf("no shipped swarms found: %v", err)
	}
	for _, f := range swarms {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sw, err := registry.ParseSwarmMarkdown(raw, filepath.Base(f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		files++
		roles += len(sw.Roles)
		for _, p := range pinsOf(sw) {
			t.Errorf("%s: %s", filepath.Base(f), p)
		}
		for _, r := range sw.Roles {
			if tierExempt[sw.ID+"/"+r.Name] {
				continue
			}
			if !rolelibrary.ValidModelTier(r.ModelTier) {
				t.Errorf("%s: role %s names no valid modelTier (%q)", filepath.Base(f), r.Name, r.ModelTier)
			}
		}
	}

	// configs/project-templates/*/swarm.md.tmpl: a model line may only be a
	// placeholder for a parameter whose default is empty, so a project created
	// with the defaults inherits the setup's model.
	cat, err := templates.Load(filepath.Join(configsRoot, "project-templates"))
	if err != nil {
		t.Fatal(err)
	}
	// Walk the directories, not cat.List(): List leaves out hidden templates
	// (custom-base), which still ship.
	tmplSwarms, err := filepath.Glob(filepath.Join(configsRoot, "project-templates", "*", "swarm.md.tmpl"))
	if err != nil || len(tmplSwarms) == 0 {
		t.Fatalf("no template swarms found: %v", err)
	}
	for _, f := range tmplSwarms {
		m, ok := cat.Get(filepath.Base(filepath.Dir(f)))
		if !ok {
			t.Fatalf("%s: no template.yaml manifest", f)
		}
		defaults := map[string]string{}
		for _, p := range m.Parameters {
			defaults[p.Name] = p.Default
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		files++
		names := roleNameRe.FindAllStringSubmatch(string(raw), -1)
		tiers := tierLineRe.FindAllStringSubmatch(string(raw), -1)
		if len(tiers) != len(names) {
			t.Errorf("template %s: %d roles but %d modelTier lines", m.Slug, len(names), len(tiers))
		}
		for _, tl := range tiers {
			if !rolelibrary.ValidModelTier(tl[1]) {
				t.Errorf("template %s: modelTier %q is not a tier", m.Slug, tl[1])
			}
		}
		for _, line := range modelLineRe.FindAllStringSubmatch(string(raw), -1) {
			roles++
			ph := placeholderRe.FindStringSubmatch(line[2])
			switch {
			case ph == nil:
				t.Errorf("template %s: %s names a model: %s", m.Slug, line[1], strings.TrimSpace(line[2]))
			case defaults[ph[1]] != "":
				t.Errorf("template %s: %s defaults to %q through {{.%s}}", m.Slug, line[1], defaults[ph[1]], ph[1])
			}
		}
	}

	// The hallucination judge in shipped project templates and examples.
	var projectFiles []string
	for _, g := range []string{"projects/*.template", "examples/*.yaml"} {
		got, err := filepath.Glob(filepath.Join(configsRoot, g))
		if err != nil || len(got) == 0 {
			t.Fatalf("no shipped project files match %s: %v", g, err)
		}
		projectFiles = append(projectFiles, got...)
	}
	for _, f := range projectFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			HallucinationJudge struct {
				Model string `yaml:"model"`
			} `yaml:"hallucinationJudge"`
		}
		// The YAML parse is the detector. A project template may hold {{...}}
		// markers YAML cannot read; only such a file falls back to the judge
		// block's text.
		if yaml.Unmarshal(raw, &doc) != nil {
			if judgeModelRe.Match(raw) {
				t.Errorf("%s: hallucinationJudge names a model", filepath.Base(f))
			}
		} else if doc.HallucinationJudge.Model != "" {
			t.Errorf("%s: hallucinationJudge.model %s", filepath.Base(f), doc.HallucinationJudge.Model)
		}
		files++
	}

	t.Logf("examined %d shipped files with %d swarm roles", files, roles)
	if roles == 0 {
		t.Fatal("no roles examined: the walk found nothing")
	}
}

// tierExempt lists the roles that name no tier. The assistant swarm's
// dispatcher is a billing stub: it mirrors chat.model and runs nothing.
var tierExempt = map[string]bool{"assistant-swarm/dispatcher": true}

// roleNameRe is a top-level role in a template swarm; tierLineRe its tier.
var (
	roleNameRe = regexp.MustCompile(`(?m)^  - name:\s*"?[\w-]+"?\s*$`)
	tierLineRe = regexp.MustCompile(`(?m)^    modelTier:\s*"?([\w-]*)"?\s*$`)
)

// modelLineRe is a role's model or fallback line in a template swarm.
var modelLineRe = regexp.MustCompile(`(?m)^[ \t]+(model|modelFallback):[ \t]*(.*)$`)

// placeholderRe is a whole-value template placeholder: "{{.name}}".
var placeholderRe = regexp.MustCompile(`^"?\{\{\s*\.([A-Za-z0-9_]+)\s*\}\}"?\s*$`)

// judgeModelRe finds a model line inside a hallucinationJudge block.
var judgeModelRe = regexp.MustCompile(`(?m)^hallucinationJudge:\n(?:[ \t]+.*\n|[ \t]*#.*\n|\n)*?[ \t]+model:[ \t]*"?[^"\s{]`)
