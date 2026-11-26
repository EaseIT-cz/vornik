package api

// GitHub #621, LLD 28: the Cursor companion bundle. These pin what the design
// promises about the bundle itself; the connect step is tested in internal/cli.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cursorBundleDir = "../../contrib/cursor-companion"

// B3: the always-applied rule ends with a byte-equal copy of the daemon's
// working rules, so Cursor's model reads what every companion session gets.
func TestCursorBundle_RuleEndsWithTheDaemonGuidance(t *testing.T) {
	rule, err := os.ReadFile(filepath.Join(cursorBundleDir, "rules", "vornik.mdc"))
	require.NoError(t, err)
	guidance, err := os.ReadFile("companion_guidance.md")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(rule), string(guidance)),
		"rules/vornik.mdc must end with internal/api/companion_guidance.md, byte for byte")
	assert.True(t, strings.HasPrefix(string(rule), "---\n"), "the rule needs front matter")
	assert.Contains(t, string(rule), "\nalwaysApply: true\n")
	assert.Contains(t, string(rule), "recent_memory", "the fallback for a session hook that does not run")
}

// B1: Cursor's review rule: manifest paths are relative and never leave the
// plugin directory, and each names something that exists.
func TestCursorBundle_ManifestPathsAreRelativeAndExist(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(cursorBundleDir, ".cursor-plugin", "plugin.json"))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, "vornik-companion", m["name"])
	for _, key := range []string{"rules", "skills", "mcpServers", "hooks"} {
		p, ok := m[key].(string)
		require.Truef(t, ok, "manifest %s must be a path", key)
		assert.Truef(t, strings.HasPrefix(p, "./"), "%s = %q must be relative", key, p)
		assert.NotContainsf(t, p, "..", "%s = %q must not leave the plugin", key, p)
		_, err := os.Stat(filepath.Join(cursorBundleDir, p))
		assert.NoErrorf(t, err, "%s = %q must exist", key, p)
	}
}

// B2: the one MCP server is the stdio bridge, and nothing in the bundle holds
// a key or a header that would carry one.
func TestCursorBundle_MCPEntryIsTheBridgeAndHoldsNoKey(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(cursorBundleDir, "mcp.json"))
	require.NoError(t, err)
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	require.Len(t, cfg.MCPServers, 1)
	vornik, ok := cfg.MCPServers["vornik"]
	require.True(t, ok, "the server is named vornik")
	assert.Equal(t, "vornikctl", vornik["command"])
	assert.Equal(t, []any{"companion", "mcp-bridge", "--name", "vornik"}, vornik["args"])
	for _, forbidden := range []string{"url", "headers", "env"} {
		_, has := vornik[forbidden]
		assert.Falsef(t, has, "the bridge entry must not carry %q", forbidden)
	}
	assert.NotRegexp(t, regexp.MustCompile(`sk-vornik-|Bearer|TOKEN`), string(raw))
}

// B4: the skills are the Codex bundle's, rewritten by skills-from-codex.sed
// and nothing else. Run ./sync-skills.sh after editing a Codex skill.
func TestCursorBundle_SkillsAreTheCodexCopiesRewritten(t *testing.T) {
	if _, err := exec.LookPath("sed"); err != nil {
		t.Skip("sed not on PATH")
	}
	codexSkills, err := filepath.Glob("../../contrib/codex-companion/skills/*/SKILL.md")
	require.NoError(t, err)
	require.NotEmpty(t, codexSkills)
	cursorSkills, err := filepath.Glob(filepath.Join(cursorBundleDir, "skills", "*", "SKILL.md"))
	require.NoError(t, err)
	require.Len(t, cursorSkills, len(codexSkills), "the Cursor bundle carries every Codex skill and no other")
	for _, src := range codexSkills {
		name := filepath.Base(filepath.Dir(src))
		want, err := exec.Command("sed", "-f", filepath.Join(cursorBundleDir, "skills-from-codex.sed"), src).Output()
		require.NoError(t, err, name)
		got, err := os.ReadFile(filepath.Join(cursorBundleDir, "skills", name, "SKILL.md"))
		require.NoErrorf(t, err, "skill %s missing from the Cursor bundle", name)
		assert.Equalf(t, string(want), string(got), "skills/%s differs from its rewritten Codex copy: run contrib/cursor-companion/sync-skills.sh", name)
	}
}

// B4: Cursor's naming of MCP tools is unverified, so no file in the bundle
// names a prefixed tool; a later edit cannot plant a guessed prefix.
func TestCursorBundle_NamesNoPrefixedTool(t *testing.T) {
	err := filepath.WalkDir(cursorBundleDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".sed") {
			return err
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

// The backlog-deposit skill drives ./scripts/vornik-backlog-deposit.sh, the
// same script the other bundles ship.
func TestCursorBundle_BacklogScriptIsTheSharedCopy(t *testing.T) {
	want, err := os.ReadFile("../../contrib/codex-companion/scripts/vornik-backlog-deposit.sh")
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(cursorBundleDir, "scripts", "vornik-backlog-deposit.sh"))
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}
