package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// GitHub #362 (found by companion review task_20261013104015_f8d49f7b5b6ad4b7,
// finding 1): the Claude Code bundle told the model to call mcp__vornik__<tool>,
// but a plugin-installed server's tools are named
// mcp__plugin_vornik-companion_vornik__<tool>, and there is no alias. A model
// following the planted name asked for a tool that does not exist, and every
// allowed-tools entry pre-approved nothing.
//
// This guard derives what the harness exposes, never from a hand list:
//   - the qualified prefix from the bundle's own manifest and .mcp.json;
//   - the tool set from the daemon's companion palette.
//
// LLD 2026-08-20 companion hardening §11.5.

// mcpNameRe matches one qualified MCP tool reference. The trailing
// lowercase/underscore run is the tool; everything between "mcp__" and the
// last "__" is the server part. A name never ends in "." or "-", so prose
// punctuation after a name ("on mcp__vornik__recall.") is not part of it
// (wave 6 merge 2026-10-13: the Codex recall-nudge comment read as a bad name).
var mcpNameRe = regexp.MustCompile(`mcp__[A-Za-z0-9_.-]*[A-Za-z0-9_]`)

// codexSanitize is the character class Codex replaces in a tool namespace.
var codexSanitize = regexp.MustCompile(`[^A-Za-z0-9_]`)

// companionPaletteNames is every tool name the companion endpoint can offer:
// the base palette plus the agent-admin tools.
func companionPaletteNames() map[string]bool {
	names := map[string]bool{}
	for _, d := range companionToolDefs() {
		names[d.Name] = true
	}
	for _, d := range companionAdminToolDefs() {
		names[d.Name] = true
	}
	return names
}

func readMCPServerKeys(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read %s", path)
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	require.NoErrorf(t, json.Unmarshal(raw, &cfg), "parse %s", path)
	require.NotEmptyf(t, cfg.MCPServers, "%s declares no MCP server", path)
	keys := make([]string, 0, len(cfg.MCPServers))
	for k := range cfg.MCPServers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// splitQualified splits "mcp__<server>__<tool>" at the LAST "__". ok is
// false when there is no server part.
func splitQualified(name string) (server, tool string, ok bool) {
	rest := strings.TrimPrefix(name, "mcp__")
	i := strings.LastIndex(rest, "__")
	if i <= 0 {
		return "", "", false
	}
	return rest[:i], rest[i+2:], true
}

type bundleNameRef struct {
	file string
	line int
	name string
}

// scanBundleMCPNames returns every mcp__ token in the bundle's files, test
// files and the manifest excluded (fixtures and changelog prose, not text a
// model is told to act on), and how many files it read.
func scanBundleMCPNames(t *testing.T, root string) (refs []bundleNameRef, files int) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()
		if strings.Contains(base, "_test.") || base == "plugin.json" || base == "LICENSE" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		for i, line := range strings.Split(string(raw), "\n") {
			for _, m := range mcpNameRe.FindAllString(line, -1) {
				refs = append(refs, bundleNameRef{file: path, line: i + 1, name: m})
			}
		}
		return nil
	})
	require.NoError(t, err)
	return refs, files
}

// hooksMatcherNames maps each qualified name in a hooks.json matcher to the
// full set of names listed by the same matcher.
func hooksMatcherNames(t *testing.T, path string) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read %s", path)
	var cfg struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	out := map[string]map[string]bool{}
	for _, entries := range cfg.Hooks {
		for _, e := range entries {
			set := map[string]bool{}
			for _, alt := range strings.Split(e.Matcher, "|") {
				set[alt] = true
			}
			for alt := range set {
				if strings.HasPrefix(alt, "mcp__") {
					out[alt] = set
				}
			}
		}
	}
	return out
}

func TestCompanionBundles_MCPToolNamesResolveAsInstalled(t *testing.T) {
	palette := companionPaletteNames()
	require.NotEmpty(t, palette)

	t.Run("claude-code", func(t *testing.T) {
		root := "../../contrib/claude-code-companion"
		raw, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
		require.NoError(t, err)
		var manifest struct {
			Name string `json:"name"`
		}
		require.NoError(t, json.Unmarshal(raw, &manifest))
		require.NotEmpty(t, manifest.Name)

		// Claude Code names a plugin's MCP tool
		// mcp__plugin_<plugin>_<server>__<tool> (LLD §9.4, measured).
		pluginServers := map[string]string{} // qualified server part -> .mcp.json key
		for _, k := range readMCPServerKeys(t, filepath.Join(root, ".mcp.json")) {
			pluginServers["plugin_"+manifest.Name+"_"+k] = k
		}
		hooksPath := filepath.Join(root, "hooks", "hooks.json")
		matchers := hooksMatcherNames(t, hooksPath)

		refs, files := scanBundleMCPNames(t, root)
		t.Logf("claude-code: scanned %d files, checked %d mcp__ names against %d palette tools",
			files, len(refs), len(palette))
		require.NotEmpty(t, refs, "checked zero names: the scan looked at nothing")

		var bad []string
		for _, r := range refs {
			server, tool, ok := splitQualified(r.name)
			switch {
			case !ok || !palette[tool]:
				bad = append(bad, describeBad(r, "not a tool the companion endpoint offers"))
			case pluginServers[server] != "":
				// The name the model sees for a plugin install.
			case filepath.Clean(r.file) == filepath.Clean(hooksPath) && isServerKey(pluginServers, server) &&
				matchers[r.name] != nil && matchers[r.name]["mcp__"+pluginPrefixFor(pluginServers, server)+"__"+tool]:
				// §11.3 exception: a hand-added server's name, in a hooks.json
				// matcher, beside the plugin form of the same tool.
			default:
				bad = append(bad, describeBad(r, "not the plugin-qualified name; Claude Code exposes "+
					"mcp__"+anyPluginServer(pluginServers)+"__"+tool))
			}
		}
		require.Emptyf(t, bad, "%d of %d tool names in the Claude Code bundle cannot be called as installed:\n%s",
			len(bad), len(refs), strings.Join(bad, "\n"))
	})

	t.Run("codex", func(t *testing.T) {
		root := "../../contrib/codex-companion"
		// Codex registers a plugin's server under its own .mcp.json key (the
		// plugin is attribution, not part of the name) and names its tools
		// mcp__<server>__<tool>, the server sanitised to [A-Za-z0-9_]
		// (LLD §11.2: openai/codex qualified_mcp_tool_name_prefix).
		servers := map[string]bool{}
		for _, k := range readMCPServerKeys(t, filepath.Join(root, ".mcp.json")) {
			servers[codexSanitize.ReplaceAllString(k, "_")] = true
		}
		refs, files := scanBundleMCPNames(t, root)
		t.Logf("codex: scanned %d files, checked %d mcp__ names against %d palette tools",
			files, len(refs), len(palette))
		require.NotEmpty(t, refs, "checked zero names: the scan looked at nothing")

		var bad []string
		for _, r := range refs {
			server, tool, ok := splitQualified(r.name)
			if !ok || !palette[tool] {
				bad = append(bad, describeBad(r, "not a tool the companion endpoint offers"))
				continue
			}
			if !servers[server] {
				bad = append(bad, describeBad(r, "server part is not a server the bundle's .mcp.json declares"))
			}
		}
		require.Emptyf(t, bad, "%d of %d tool names in the Codex bundle cannot be called as installed:\n%s",
			len(bad), len(refs), strings.Join(bad, "\n"))
	})
}

// TestCodexSanitize_MatchesObservedNamespace pins the guard's Codex server
// rule on the one observed hyphenated key: server "vornik-codex" is exposed
// as namespace mcp__vornik_codex (LLD §11.2/§11.5, review round 2 F1), so an
// unsanitised mcp__vornik-codex__<tool> in a bundle is refused.
func TestCodexSanitize_MatchesObservedNamespace(t *testing.T) {
	require.Equal(t, "vornik_codex", codexSanitize.ReplaceAllString("vornik-codex", "_"))
	require.Equal(t, "vornik", codexSanitize.ReplaceAllString("vornik", "_"))
	server, tool, ok := splitQualified("mcp__vornik-codex__recall")
	require.True(t, ok)
	require.Equal(t, "recall", tool)
	require.NotEqual(t, codexSanitize.ReplaceAllString("vornik-codex", "_"), server,
		"the unsanitised server part must not match the sanitised key")
}

func describeBad(r bundleNameRef, why string) string {
	return "  " + r.file + ":" + strconv.Itoa(r.line) + ": " + r.name + " — " + why
}

// isServerKey reports whether server is a bare .mcp.json key (the name a
// hand-added server with the same key would carry).
func isServerKey(pluginServers map[string]string, server string) bool {
	for _, k := range pluginServers {
		if k == server {
			return true
		}
	}
	return false
}

func pluginPrefixFor(pluginServers map[string]string, key string) string {
	for q, k := range pluginServers {
		if k == key {
			return q
		}
	}
	return ""
}

func anyPluginServer(pluginServers map[string]string) string {
	qs := make([]string, 0, len(pluginServers))
	for q := range pluginServers {
		qs = append(qs, q)
	}
	sort.Strings(qs)
	return strings.Join(qs, "|")
}
