package api

// GitHub #841, LLD 29: the opencode companion bundle. These pin what the
// design promises about the bundle itself; the connect step is tested in
// internal/cli.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/testutil/repowalk"
)

const opencodeBundleDir = "../../contrib/opencode-companion"

// B3: the guidance the plugin appends ends with a byte-equal copy of the
// daemon's working rules, and starts with the marker connect checks.
func TestOpenCodeBundle_GuidanceEndsWithTheDaemonGuidance(t *testing.T) {
	g, err := os.ReadFile(filepath.Join(opencodeBundleDir, "plugin", "vornik-companion-guidance.md"))
	require.NoError(t, err)
	guidance, err := os.ReadFile("companion_guidance.md")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(g), string(guidance)), "the guidance must end with internal/api/companion_guidance.md, byte for byte")
	assert.True(t, strings.HasPrefix(string(g), "<!-- vornik-companion"), "the marker connect checks before replacing the file")
	assert.Contains(t, string(g), "recent_memory", "the fallback for a digest that is unavailable")
}

// B3/B7: the plugin starts with the marker, holds each constant connect
// rewrites exactly once, and carries no key or header.
func TestOpenCodeBundle_PluginShape(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(opencodeBundleDir, "plugin", "vornik-companion.js"))
	require.NoError(t, err)
	js := string(raw)
	assert.True(t, strings.HasPrefix(js, "// vornik-companion"), "the marker connect checks before replacing the file")
	for _, c := range []string{`const VORNIKCTL = "vornikctl";`, `const KEY_NAME = "default";`, `const VORNIK_URL = "";`, `const VERSION = "`, `const TESTED_OPENCODE = "`} {
		assert.Equalf(t, 1, strings.Count(js, c), "the plugin must hold %q exactly once", c)
	}
	assert.NotRegexp(t, regexp.MustCompile(`sk-vornik-|Bearer|VORNIK_COMPANION_TOKEN`), js)
	assert.Contains(t, js, "experimental.chat.system.transform")
	assert.Contains(t, js, "output.system[output.system.length - 1] +=", "append to the last system string; a pushed entry does not reach the model (LLD 29 A2)")
	if _, err := exec.LookPath("node"); err == nil {
		out, err := exec.Command("node", "--check", filepath.Join(opencodeBundleDir, "plugin", "vornik-companion.js")).CombinedOutput()
		assert.NoErrorf(t, err, "plugin syntax: %s", out)
	}
}

// B1: the skills are the Codex bundle's, rewritten by skills-from-codex.sed
// and nothing else, under vornik- names. Run ./sync-skills.sh after editing a
// Codex skill.
func TestOpenCodeBundle_SkillsAreTheCodexCopiesRewritten(t *testing.T) {
	if _, err := exec.LookPath("sed"); err != nil {
		t.Skip("sed not on PATH")
	}
	codexSkills, err := filepath.Glob("../../contrib/codex-companion/skills/*/SKILL.md")
	require.NoError(t, err)
	require.NotEmpty(t, codexSkills)
	ocSkills, err := filepath.Glob(filepath.Join(opencodeBundleDir, "skills", "*", "SKILL.md"))
	require.NoError(t, err)
	require.Len(t, ocSkills, len(codexSkills), "the opencode bundle carries every Codex skill and no other")
	for _, src := range codexSkills {
		name := filepath.Base(filepath.Dir(src))
		dst := name
		if !strings.HasPrefix(name, "vornik-") {
			dst = "vornik-" + name
		}
		want, err := exec.Command("sed", "-f", filepath.Join(opencodeBundleDir, "skills-from-codex.sed"), src).Output()
		require.NoError(t, err, name)
		got, err := os.ReadFile(filepath.Join(opencodeBundleDir, "skills", dst, "SKILL.md"))
		require.NoErrorf(t, err, "skill %s missing from the opencode bundle", dst)
		assert.Equalf(t, string(want), string(got), "skills/%s differs from its rewritten Codex copy: run contrib/opencode-companion/sync-skills.sh", dst)
		assert.Containsf(t, string(got), "\nname: "+dst+"\n", "skills/%s: its front-matter name must match its directory", dst)
	}
}

// opencode's tool naming follows the entry's name, so no file in the bundle
// names a prefixed tool.
func TestOpenCodeBundle_NamesNoPrefixedTool(t *testing.T) {
	err := filepath.WalkDir(opencodeBundleDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(opencodeBundleDir, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".sed") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if m := mcpNameRe.Find(raw); m != nil {
			t.Errorf("%s names the prefixed tool %q", path, m)
		}
		return nil
	})
	require.NoError(t, err)
}

func TestOpenCodeBundle_BacklogScriptIsTheSharedCopy(t *testing.T) {
	want, err := os.ReadFile("../../contrib/codex-companion/scripts/vornik-backlog-deposit.sh")
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(opencodeBundleDir, "scripts", "vornik-backlog-deposit.sh"))
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}
