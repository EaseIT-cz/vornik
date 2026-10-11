package api

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// HARNESS PARITY (operator, 2026-10-08; LLD 21 §D2 "The generic harness
// contract", D5): every user-visible companion or agent-admin tool reaches all
// three shipped bundles, or the gap is written down with its reason. The
// manifest is internal/api/testdata/harness_parity.yaml; a later feature adds
// its capability there in the same change that adds the tool.
//
// What this proves: each bundle mentions the capability in the file that
// teaches it. It does not prove that the text is right or that a harness calls
// the tool; the e2e lanes in test/e2e/hermes exercise the calls.

// parityBundles are the shipped bundles, in a fixed order.
var parityBundles = []string{"claude-code", "codex", "hermes"}

type paritySurface struct {
	File string `yaml:"file"`
	Says string `yaml:"says"`
}

type parityCapability struct {
	Name          string                   `yaml:"name"`
	Tools         []string                 `yaml:"tools"`
	Bundles       map[string]paritySurface `yaml:"bundles"`
	ExemptBundles map[string]string        `yaml:"exempt_bundles"`
}

type parityExempt struct {
	Tool   string `yaml:"tool"`
	Reason string `yaml:"reason"`
}

type parityManifest struct {
	Capabilities []parityCapability `yaml:"capabilities"`
	Exempt       []parityExempt     `yaml:"exempt"`
}

// parityManifestPath and parityRoot: the manifest and the repository root
// its file paths are relative to.
const (
	parityManifestPath = "testdata/harness_parity.yaml"
	parityRoot         = "../.."
)

func loadParityManifest(t *testing.T) parityManifest {
	t.Helper()
	path := parityManifestPath
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m parityManifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func (m parityManifest) toolSet() map[string]bool {
	out := map[string]bool{}
	for _, c := range m.Capabilities {
		for _, tool := range c.Tools {
			out[tool] = true
		}
	}
	return out
}

func (m parityManifest) exempt(tool string) bool {
	for _, e := range m.Exempt {
		if e.Tool == tool && strings.TrimSpace(e.Reason) != "" {
			return true
		}
	}
	return false
}

// parityToolUniverse is every tool the companion endpoint can offer a key:
// the companion tools and the agent-admin verbs.
func parityToolUniverse() map[string]bool {
	tools := map[string]bool{}
	for _, d := range companionToolDefs() {
		tools[d.Name] = true
	}
	for _, d := range companionAdminToolDefs() {
		tools[d.Name] = true
	}
	return tools
}

// checkParity returns the violations of the manifest against the tool
// universe and the bundle files under the repository root. Split from the test so the test
// can show it fails on a broken manifest.
func checkParity(m parityManifest, tools map[string]bool) []string {
	var errs []string
	listed := m.toolSet()
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !listed[name] && !m.exempt(name) {
			errs = append(errs, "tool "+name+" is in no capability and not exempt")
		}
	}
	for name := range listed {
		if !tools[name] {
			errs = append(errs, "manifest names tool "+name+", which the daemon does not offer")
		}
	}
	for _, e := range m.Exempt {
		if strings.TrimSpace(e.Reason) == "" {
			errs = append(errs, "exempt tool "+e.Tool+" has no reason")
		}
	}
	for _, c := range m.Capabilities {
		for _, b := range parityBundles {
			s, ok := c.Bundles[b]
			reason := strings.TrimSpace(c.ExemptBundles[b])
			switch {
			case ok && reason != "":
				errs = append(errs, "capability "+c.Name+" has both a "+b+" surface and an exemption")
			case !ok && reason == "":
				errs = append(errs, "capability "+c.Name+" has no "+b+" surface and no exemption")
			case ok:
				raw, err := os.ReadFile(filepath.Join(parityRoot, s.File))
				if err != nil {
					errs = append(errs, "capability "+c.Name+": "+b+" file "+s.File+" does not exist")
					continue
				}
				if s.Says == "" || !strings.Contains(string(raw), s.Says) {
					errs = append(errs, "capability "+c.Name+": "+s.File+" does not say "+`"`+s.Says+`"`)
				}
			}
		}
	}
	return errs
}

func TestHarnessParity_EveryCapabilityReachesEveryBundle(t *testing.T) {
	m := loadParityManifest(t)
	tools := parityToolUniverse()
	for _, e := range checkParity(m, tools) {
		t.Error(e)
	}
	t.Logf("examined %d tools, %d capabilities, %d bundles (%d tools exempt)",
		len(tools), len(m.Capabilities), len(parityBundles), len(m.Exempt))
}

// The check can fail: a capability that loses its Hermes surface, a tool the
// manifest forgets, and a phrase the file no longer says are each reported.
func TestHarnessParity_CheckFailsOnAGap(t *testing.T) {
	m := loadParityManifest(t)
	tools := parityToolUniverse()

	missingTool := map[string]bool{"a_tool_nobody_listed": true}
	for k := range tools {
		missingTool[k] = true
	}
	if errs := checkParity(m, missingTool); len(errs) == 0 {
		t.Error("a tool in no capability passed")
	}

	broken := loadParityManifest(t)
	for i := range broken.Capabilities {
		if broken.Capabilities[i].Name == "delegate-work" {
			delete(broken.Capabilities[i].Bundles, "hermes")
		}
	}
	if errs := checkParity(broken, tools); len(errs) == 0 {
		t.Error("a capability with no hermes surface and no exemption passed")
	}

	says := loadParityManifest(t)
	for i := range says.Capabilities {
		if says.Capabilities[i].Name == "delegate-work" {
			s := says.Capabilities[i].Bundles["hermes"]
			s.Says = "vornik_delegate_that_the_skill_never_says"
			says.Capabilities[i].Bundles["hermes"] = s
		}
	}
	if errs := checkParity(says, tools); len(errs) == 0 {
		t.Error("a phrase the file does not say passed")
	}
}

// Every file the manifest names exists (D5 Step 4).
func TestHarnessParity_ManifestFilesExist(t *testing.T) {
	m := loadParityManifest(t)
	n := 0
	for _, c := range m.Capabilities {
		for b, s := range c.Bundles {
			n++
			if _, err := os.Stat(filepath.Join(parityRoot, s.File)); err != nil {
				t.Errorf("capability %s, bundle %s: %s does not exist", c.Name, b, s.File)
			}
		}
	}
	t.Logf("examined %d bundle surfaces", n)
}
