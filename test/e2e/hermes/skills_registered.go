package hermes

// H0-skills: the plugin's skills are registered in Hermes (lane design,
// "Amendment 2026-10-16 — H0-skills", GitHub #388). Untagged so the unit
// suite reaches it; scenarios_test.go runs the probe.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// skillsMarker starts the probe's one result line.
const skillsMarker = "VORNIK_SKILLS "

// skillsProbe runs in the Hermes image the way a `hermes -z` start reaches
// the skill registry: general plugin discovery, then the configured memory
// provider (agent/agent_init.py), whose register_skill wrapper swallows
// errors at debug level; then the model's own skills_list tool. The provider
// name is read from config.yaml by Hermes's own reader, as agent_init does.
const skillsProbe = `import json
from hermes_cli.plugins import discover_plugins
from plugins.memory import _get_active_memory_provider, load_memory_provider
from tools.skills_tool import skills_list
discover_plugins()
name = _get_active_memory_provider() or ""
provider = load_memory_provider(name) if name else None
res = json.loads(skills_list())
print("` + skillsMarker + `" + json.dumps({"success": res.get("success"), "memory_provider": name, "provider_loaded": provider is not None, "skills": [s.get("name") for s in res.get("skills", [])] if "skills" in res else None}))
`

// expectedPluginSkills derives the skills the bundle at dir must register,
// as Hermes keys them (<namespace>:<name>): the namespace is plugin.yaml's
// skill_namespace or else its name, and every directory under skills/ is a
// skill whose SKILL.md frontmatter name equals the directory name. Anything
// that would let the check examine nothing, or the wrong name, is an error.
func expectedPluginSkills(dir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Name           string `yaml:"name"`
		SkillNamespace string `yaml:"skill_namespace"`
	}
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("plugin.yaml: %w", err)
	}
	namespace := manifest.SkillNamespace
	if namespace == "" {
		namespace = manifest.Name
	}
	if namespace == "" {
		return nil, fmt.Errorf("plugin.yaml sets neither skill_namespace nor name")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "skills"))
	if err != nil {
		return nil, fmt.Errorf("the bundle's skills directory: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		md, err := os.ReadFile(filepath.Join(dir, "skills", e.Name(), "SKILL.md"))
		if err != nil {
			return nil, fmt.Errorf("skills/%s has no SKILL.md: %w", e.Name(), err)
		}
		name, err := frontmatterName(md)
		if err != nil {
			return nil, fmt.Errorf("skills/%s/SKILL.md: %w", e.Name(), err)
		}
		if name != e.Name() {
			return nil, fmt.Errorf("skills/%s/SKILL.md: frontmatter name %q differs from its directory", e.Name(), name)
		}
		out = append(out, namespace+":"+name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the bundle's skills directory holds no skill")
	}
	slices.Sort(out)
	return out, nil
}

// frontmatterName returns the name field of a SKILL.md's YAML frontmatter.
func frontmatterName(md []byte) (string, error) {
	md = bytes.TrimPrefix(md, []byte{0xef, 0xbb, 0xbf}) // a UTF-8 BOM
	rest, ok := bytes.CutPrefix(md, []byte("---\n"))
	if !ok {
		return "", fmt.Errorf("no frontmatter")
	}
	block, _, ok := bytes.Cut(rest, []byte("\n---"))
	if !ok {
		return "", fmt.Errorf("unterminated frontmatter")
	}
	var fm struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(block, &fm); err != nil {
		return "", fmt.Errorf("frontmatter: %w", err)
	}
	if fm.Name == "" {
		return "", fmt.Errorf("frontmatter has no name")
	}
	return fm.Name, nil
}

// checkPluginSkills judges the probe's output against the expected set. It
// returns a summary stating how many expected skills it checked; a probe
// that printed no result, or reported failure, is an error, never a pass.
func checkPluginSkills(expected []string, probeOut string) (string, error) {
	if len(expected) == 0 {
		return "", fmt.Errorf("no expected skills: nothing would be checked")
	}
	var line string
	for _, l := range strings.Split(probeOut, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), skillsMarker); ok {
			line = rest
		}
	}
	if line == "" {
		return "", fmt.Errorf("the probe printed no %q line; output:\n%s", strings.TrimSpace(skillsMarker), probeOut)
	}
	var res struct {
		Success        *bool     `json:"success"`
		MemoryProvider string    `json:"memory_provider"`
		ProviderLoaded bool      `json:"provider_loaded"`
		Skills         *[]string `json:"skills"`
	}
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		return "", fmt.Errorf("the probe's result line: %w: %s", err, line)
	}
	if res.Success == nil || !*res.Success || res.Skills == nil {
		return "", fmt.Errorf("skills_list did not succeed: %s", line)
	}
	namespace, _, _ := strings.Cut(expected[0], ":")
	// The memory provider keys its skills by its own name, general discovery
	// by the manifest's namespace: only when they agree does one expected
	// name cover both registration paths (review-20261016-7094 suggestion 2).
	if res.MemoryProvider != namespace {
		return "", fmt.Errorf("memory.provider is %q, not the plugin's skill namespace %q: the memory provider's skills would be keyed differently", res.MemoryProvider, namespace)
	}
	var listed, extra, missing []string
	for _, s := range *res.Skills {
		if strings.HasPrefix(s, namespace+":") {
			listed = append(listed, s)
			if !slices.Contains(expected, s) {
				extra = append(extra, s)
			}
		}
	}
	for _, want := range expected {
		if !slices.Contains(*res.Skills, want) {
			missing = append(missing, want)
		}
	}
	// provider_loaded is reported, not judged: a provider that did not load
	// is H3's failure (no recall), and general discovery may still have
	// registered every skill, which is what the model would see.
	summary := fmt.Sprintf("checked %d expected skills from the bundle's skills directory, %d listed under %s (memory provider loaded: %t)",
		len(expected), len(listed), namespace, res.ProviderLoaded)
	if len(extra) > 0 {
		summary += fmt.Sprintf("; listed but not in the bundle's skills directory: %v", extra)
	}
	if len(missing) > 0 {
		return summary, fmt.Errorf("%s; not registered: %v", summary, missing)
	}
	return summary, nil
}
