package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #305 (2026-10-08): every companion tool decoded its arguments with
// json.Unmarshal, which drops a field the tool does not declare, so a wrong
// or unsupported argument was answered as if it had not been sent (list's
// "project" from Hermes). Plan fork F8: all of them are strict now. LLD 21 §A,
// "list takes a project, and tool arguments are strict".

const unknownProbeField = "zz_not_an_argument"

func TestDecodeToolArgs(t *testing.T) {
	var a listArgs
	for _, raw := range []string{"", "null", "  ", "{}"} {
		require.NoError(t, decodeToolArgs("list", json.RawMessage(raw), &a), "raw %q", raw)
	}
	require.NoError(t, decodeToolArgs("list", json.RawMessage(`{"limit":3,"status":"QUEUED","project":"p"}`), &a))
	assert.Equal(t, listArgs{Limit: 3, Status: "QUEUED", Project: "p"}, a)

	err := decodeToolArgs("list", json.RawMessage(`{"projekt":"x"}`), &listArgs{})
	require.Error(t, err)
	assert.Equal(t, `list: unknown argument "projekt"; this tool accepts: limit, status, project`, err.Error())

	err = decodeToolArgs("catalog", json.RawMessage(`{"x":1}`), &noArgs{})
	require.Error(t, err)
	assert.Equal(t, `catalog: unknown argument "x"; this tool takes no arguments`, err.Error())

	err = decodeToolArgs("list", json.RawMessage(`{"limit":"ten"}`), &listArgs{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid arguments")

	err = decodeToolArgs("list", json.RawMessage(`{} {}`), &listArgs{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailing data")
}

// Every tool the endpoint offers has an argument type, and a call carrying
// an unknown field is refused through the real handler, naming the field.
func TestCompanionStrictArgs_EveryToolRefusesUnknownField(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	plainRaw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	plain := keyRepo.rows[len(keyRepo.rows)-1]
	plain.MemoryRead, plain.MemoryWrite = true, true
	plain.SkillRead, plain.SkillWrite, plain.SkillAdmin = true, true, true
	adminRaw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	keyRepo.rows[len(keyRepo.rows)-1].AgentAdmin = true
	keyRepo.rows[len(keyRepo.rows)-1].AgentNamespace = "alpha"

	examined := 0
	for _, def := range companionToolDefs() {
		examined++
		_, ok := companionToolArgs[def.Name]
		require.Truef(t, ok, "tool %s has no entry in companionToolArgs", def.Name)
		raw := plainRaw
		if agentAdminOnlyTools[def.Name] {
			raw = adminRaw
		}
		text, isErr := callTool(t, srv, raw, def.Name, map[string]any{unknownProbeField: 1})
		assert.Truef(t, isErr, "%s accepted an unknown argument: %s", def.Name, text)
		assert.Containsf(t, text, fmt.Sprintf("unknown argument %q", unknownProbeField), "%s: %s", def.Name, text)
	}
	assert.Len(t, companionToolArgs, examined, "companionToolArgs names a tool the endpoint does not offer")
	t.Logf("examined %d tool entry points", examined)
}

// A field the tool's inputSchema declares is a field the decode accepts:
// otherwise a schema-valid call would be refused.
func TestCompanionStrictArgs_SchemaFieldsAreDeclared(t *testing.T) {
	for _, def := range companionToolDefs() {
		props, _ := def.InputSchema["properties"].(map[string]any)
		newArgs, ok := companionToolArgs[def.Name]
		if !assert.Truef(t, ok, "tool %s has no entry in companionToolArgs", def.Name) {
			continue
		}
		fields := map[string]bool{}
		for _, f := range jsonFieldNames(newArgs()) {
			fields[f] = true
		}
		for p := range props {
			assert.Truef(t, fields[p], "tool %s: inputSchema declares %q but its argument type does not", def.Name, p)
		}
	}
}

type bundleCall struct {
	Bundle    string          `json:"bundle"`
	File      string          `json:"file"`
	Line      int             `json:"line"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// Call shapes the bundles use. A Python call is client.call("tool", ...),
// self._call("tool", ...) or _wrap(self.client, "tool", ...); a scripted
// tools/call body in a Claude Code command is "params": {"name": "tool";
// in a shell hook the name sits on its own line.
var (
	bundleCallPy     = regexp.MustCompile(`(?:\.call|\._call)\("([a-z_]+)"|_wrap\(self\.client, "([a-z_]+)"`)
	bundleCallParams = regexp.MustCompile(`"params": \{"name": "([a-z_]+)"`)
	bundleCallSh     = regexp.MustCompile(`^\s*"?name"?: "([a-z_]+)",\s*$`)
)

// scanBundleCalls finds every call site in the shipped bundles, keyed
// "file:line" with the tool as value.
func scanBundleCalls(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	globs := []string{
		"contrib/hermes-companion/*.py",
		"contrib/claude-code-companion/commands/*.md",
		"contrib/claude-code-companion/hooks/*.sh",
		"contrib/claude-code-companion/scripts/*.sh",
		"contrib/codex-companion/skills/*/SKILL.md",
		"contrib/codex-companion/scripts/*.sh",
	}
	for _, g := range globs {
		files, err := filepath.Glob(filepath.Join(parityRoot, g))
		require.NoError(t, err)
		for _, f := range files {
			raw, err := os.ReadFile(f)
			require.NoError(t, err)
			rel, _ := filepath.Rel(parityRoot, f)
			isSh := strings.HasSuffix(f, ".sh")
			for i, line := range strings.Split(string(raw), "\n") {
				var m []string
				switch {
				case strings.HasSuffix(f, ".py"):
					m = bundleCallPy.FindStringSubmatch(line)
				case isSh && strings.Contains(string(raw), "tools/call"):
					m = bundleCallSh.FindStringSubmatch(line)
				default:
					m = bundleCallParams.FindStringSubmatch(line)
				}
				if m == nil {
					continue
				}
				tool := m[1]
				if tool == "" && len(m) > 2 {
					tool = m[2]
				}
				found[fmt.Sprintf("%s:%d", filepath.ToSlash(rel), i+1)] = tool
			}
		}
	}
	return found
}

// Review Focus 5: no shipped bundle sends an argument the daemon does not
// declare, so no released client breaks on the strict decode.
func TestCompanionStrictArgs_ShippedBundleCallsDecode(t *testing.T) {
	raw, err := os.ReadFile("testdata/bundle_tool_calls.json")
	require.NoError(t, err)
	var fx struct {
		Calls []bundleCall `json:"calls"`
	}
	require.NoError(t, json.Unmarshal(raw, &fx))

	inFixture := map[string]string{}
	for _, c := range fx.Calls {
		newArgs, ok := companionToolArgs[c.Tool]
		require.Truef(t, ok, "%s:%d calls %s, which is not a companion tool", c.File, c.Line, c.Tool)
		assert.NoErrorf(t, decodeToolArgs(c.Tool, c.Arguments, newArgs()), "%s:%d (%s)", c.File, c.Line, c.Bundle)
		inFixture[fmt.Sprintf("%s:%d", c.File, c.Line)] = c.Tool
	}

	found := scanBundleCalls(t)
	var missing, stale []string
	for site, tool := range found {
		if inFixture[site] != tool {
			missing = append(missing, site+" ("+tool+")")
		}
	}
	for site := range inFixture {
		if _, ok := found[site]; !ok {
			stale = append(stale, site)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	assert.Empty(t, missing, "call sites in the bundles with no fixture entry: add them to testdata/bundle_tool_calls.json")
	assert.Empty(t, stale, "fixture entries whose call site moved or is gone: update the line or remove the entry")
	t.Logf("examined %d call sites found in the bundles; %d fixture entries decode", len(found), len(fx.Calls))
}
