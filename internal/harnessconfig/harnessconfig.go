// Package harnessconfig writes and removes the one MCP server entry that
// vornikctl agent connect gives a harness (agent-administered Vornik design
// §11, plan P6.2). The entry runs `vornikctl agent mcp-bridge --namespace
// <ns>` and never contains the key. Each edit merges into the harness's own
// file, keeping everything else in it, comments included where the format
// has them.
package harnessconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/agentbridge"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/config"
)

// The harnesses connect knows.
const (
	ClaudeCode    = "claude-code"
	ClaudeDesktop = "claude-desktop"
	Codex         = "codex"
	Hermes        = "hermes"
)

// Harnesses lists them in the order help text shows.
var Harnesses = []string{Hermes, ClaudeDesktop, ClaudeCode, Codex}

// Entry is the stdio MCP server entry for one namespace.
type Entry struct {
	Namespace string
	Command   string // the absolute path of vornikctl
	// URL is the daemon's base URL, passed to the bridge as --url: a harness
	// starts the bridge with its own environment, where VORNIK_API_URL is
	// not set. Empty: the bridge's default.
	URL string
}

// Name is the entry's key in the harness's server map.
func (e Entry) Name() string { return "vornik-" + e.Namespace }

// Args runs the bridge for the namespace.
func (e Entry) Args() []string {
	args := []string{"agent", "mcp-bridge", "--namespace", e.Namespace}
	if e.URL != "" {
		args = append(args, "--url", e.URL)
	}
	return args
}

func (e Entry) check() error {
	if !agentns.Valid(e.Namespace) {
		return fmt.Errorf("%q is not a namespace", e.Namespace)
	}
	if !filepath.IsAbs(e.Command) {
		return fmt.Errorf("the bridge command must be an absolute path, got %q", e.Command)
	}
	// The bridge refuses a cleartext URL off this machine too; refusing it
	// here keeps such an entry out of the harness config at all (review
	// 20261002-24b5, defence in depth).
	if e.URL != "" {
		if err := agentbridge.CheckEndpoint(e.URL); err != nil {
			return err
		}
	}
	return nil
}

// Env is where the harness files are looked up.
type Env struct {
	Home      string // the user's home directory
	ConfigDir string // os.UserConfigDir()
	Getenv    func(string) string
}

// Path is the harness's config file.
func Path(harness string, env Env) (string, error) {
	get := env.Getenv
	if get == nil {
		get = func(string) string { return "" }
	}
	switch harness {
	case ClaudeCode:
		return filepath.Join(env.Home, ".claude.json"), nil
	case ClaudeDesktop:
		return filepath.Join(env.ConfigDir, "Claude", "claude_desktop_config.json"), nil
	case Codex:
		if h := get("CODEX_HOME"); h != "" {
			return filepath.Join(h, "config.toml"), nil
		}
		return filepath.Join(env.Home, ".codex", "config.toml"), nil
	case Hermes:
		if h := get("HERMES_HOME"); h != "" {
			return filepath.Join(h, "config.yaml"), nil
		}
		return filepath.Join(env.Home, ".hermes", "config.yaml"), nil
	}
	return "", fmt.Errorf("unknown harness %q (one of %s)", harness, strings.Join(Harnesses, ", "))
}

// Set returns content with the entry added or replaced.
func Set(harness string, content []byte, e Entry) ([]byte, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	switch harness {
	case ClaudeCode, ClaudeDesktop:
		return setJSON(content, e, harness == ClaudeCode)
	case Codex:
		return setTOML(content, e)
	case Hermes:
		return setYAML(content, e)
	}
	return nil, fmt.Errorf("unknown harness %q", harness)
}

// Remove returns content without the entry, and whether it was there.
func Remove(harness string, content []byte, ns string) ([]byte, bool, error) {
	e := Entry{Namespace: ns}
	if !agentns.Valid(ns) {
		return nil, false, fmt.Errorf("%q is not a namespace", ns)
	}
	switch harness {
	case ClaudeCode, ClaudeDesktop:
		return removeJSON(content, e)
	case Codex:
		return removeTOML(content, e)
	case Hermes:
		return removeYAML(content, e)
	}
	return nil, false, fmt.Errorf("unknown harness %q", harness)
}

// ---- JSON (Claude Code, Claude Desktop): no comments to keep.

func decodeJSONObject(content []byte) (map[string]any, error) {
	m := map[string]any{}
	if len(bytes.TrimSpace(content)) == 0 {
		return m, nil
	}
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber() // a large number must come back exactly as it was
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	return m, nil
}

func encodeJSON(m map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func setJSON(content []byte, e Entry, typed bool) ([]byte, error) {
	m, err := decodeJSONObject(content)
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if m["mcpServers"] != nil && servers == nil {
		return nil, errors.New("mcpServers is not an object; edit the file by hand")
	}
	if servers == nil {
		servers = map[string]any{}
	}
	entry := map[string]any{"command": e.Command, "args": e.Args()}
	if typed {
		entry["type"] = "stdio"
	}
	servers[e.Name()] = entry
	m["mcpServers"] = servers
	return encodeJSON(m)
}

func removeJSON(content []byte, e Entry) ([]byte, bool, error) {
	m, err := decodeJSONObject(content)
	if err != nil {
		return nil, false, err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if _, ok := servers[e.Name()]; !ok {
		return content, false, nil
	}
	delete(servers, e.Name())
	out, err := encodeJSON(m)
	return out, true, err
}

// ---- YAML (Hermes): the repo's comment-preserving editor.

func setYAML(content []byte, e Entry) ([]byte, error) {
	val := map[string]any{"command": e.Command, "args": e.Args()}
	if len(bytes.TrimSpace(content)) == 0 {
		return yaml.Marshal(map[string]any{"mcp_servers": map[string]any{e.Name(): val}})
	}
	out, _, err := config.SetYAMLKey(content, "mcp_servers."+e.Name(), val)
	return out, err
}

func removeYAML(content []byte, e Entry) ([]byte, bool, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return content, false, nil
	}
	out, removed, err := config.DeleteYAMLKey(content, "mcp_servers."+e.Name())
	if err != nil || !removed {
		return content, false, err
	}
	return out, true, nil
}

// ---- TOML (Codex): the entry is one text block, so every other byte of
// the file, comments included, is kept exactly (plan P6 amendment F5; F7 of
// review 6f6b for nested tables).

func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlBlock(e Entry) string {
	args := make([]string, 0, 4)
	for _, a := range e.Args() {
		args = append(args, tomlString(a))
	}
	return "[mcp_servers." + e.Name() + "]\n" +
		"command = " + tomlString(e.Command) + "\n" +
		"args = [" + strings.Join(args, ", ") + "]\n"
}

// tomlHeader returns the table name of a header line ("" when it is not
// one) and whether it is an array of tables.
func tomlHeader(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if i := strings.Index(t, "#"); i >= 0 && !strings.Contains(t[:i], "\"") {
		t = strings.TrimSpace(t[:i])
	}
	switch {
	case strings.HasPrefix(t, "[[") && strings.HasSuffix(t, "]]"):
		return normalizeTOMLKey(t[2 : len(t)-2]), true
	case strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]"):
		return normalizeTOMLKey(t[1 : len(t)-1]), false
	}
	return "", false
}

// normalizeTOMLKey drops whitespace around dots and the quotes of a simple
// quoted segment, so [ mcp_servers . "vornik-x" ] reads as
// mcp_servers.vornik-x.
func normalizeTOMLKey(k string) string {
	parts := strings.Split(k, ".")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 2 && (p[0] == '"' && p[len(p)-1] == '"' || p[0] == '\'' && p[len(p)-1] == '\'') {
			p = p[1 : len(p)-1]
		}
		parts[i] = p
	}
	return strings.Join(parts, ".")
}

// tomlSpan finds the entry's block: from its header to the next header that
// is not the entry or a table under it. It refuses a file that defines the
// entry any other way (an array of tables, a dotted or inline key), because
// a text edit could not replace that safely.
func tomlSpan(lines []string, e Entry) (start, end int, err error) {
	own := "mcp_servers." + e.Name()
	start, end = -1, -1
	for i, l := range lines {
		name, array := tomlHeader(l)
		if name == "" {
			continue
		}
		mine := name == own || strings.HasPrefix(name, own+".")
		if mine && array {
			return 0, 0, fmt.Errorf("%s is an array of tables here; edit the file by hand", own)
		}
		switch {
		case mine && start < 0:
			if name != own {
				return 0, 0, fmt.Errorf("%s appears before its own table; edit the file by hand", name)
			}
			start = i
		case mine && end >= 0:
			return 0, 0, fmt.Errorf("%s is split across the file; edit the file by hand", own)
		case !mine && start >= 0 && end < 0:
			end = i
		}
	}
	if start >= 0 && end < 0 {
		end = len(lines)
	}
	// The name anywhere else as a key (a dotted key, an inline table) is a
	// definition a text edit would leave behind. A comment, a value that
	// mentions it, or a longer name that starts with it is not (review
	// 20261002-d930 F2).
	asKey := regexp.MustCompile(`(?:^|[\s.{,\[])["']?` + regexp.QuoteMeta(e.Name()) + `["']?\s*[=.]`)
	for i, l := range lines {
		if start >= 0 && i >= start && i < end {
			continue
		}
		if asKey.MatchString(stripTOMLComment(l)) {
			return 0, 0, fmt.Errorf("%s is defined at line %d in a form this tool does not edit; edit the file by hand", e.Name(), i+1)
		}
	}
	return start, end, nil
}

// stripTOMLComment drops a # comment that is not inside a quoted string.
func stripTOMLComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return line[:i]
		}
	}
	return line
}

func splitLines(content []byte) []string {
	s := string(content)
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

func setTOML(content []byte, e Entry) ([]byte, error) {
	lines := splitLines(content)
	start, end, err := tomlSpan(lines, e)
	if err != nil {
		return nil, err
	}
	block := strings.Split(strings.TrimSuffix(tomlBlock(e), "\n"), "\n")
	var out []string
	if start < 0 {
		out = append(out, lines...)
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, block...)
	} else {
		// Keep the blank or comment lines that close the old block, so the
		// next table keeps its separation and its leading comment.
		tail := end
		for tail > start+1 && (strings.TrimSpace(lines[tail-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[tail-1]), "#")) {
			tail--
		}
		out = append(out, lines[:start]...)
		out = append(out, block...)
		out = append(out, lines[tail:]...)
	}
	return []byte(strings.Join(out, "\n") + "\n"), nil
}

func removeTOML(content []byte, e Entry) ([]byte, bool, error) {
	lines := splitLines(content)
	start, end, err := tomlSpan(lines, e)
	if err != nil {
		return nil, false, err
	}
	if start < 0 {
		return content, false, nil
	}
	tail := end
	for tail > start+1 && (strings.TrimSpace(lines[tail-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[tail-1]), "#")) {
		tail--
	}
	rest := lines[tail:]
	if start == 0 { // the block opened the file: so does what follows it
		for len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
			rest = rest[1:]
		}
	}
	out := append(append([]string{}, lines[:start]...), rest...)
	// Drop the blank line connect put before the block, if the file now
	// ends with one.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return []byte{}, true, nil
	}
	return []byte(strings.Join(out, "\n") + "\n"), true, nil
}

// ---- Applying an edit to a file.

// ErrChanged is returned when the file kept changing under the edit.
var ErrChanged = errors.New("the file changed while it was being edited")

// fileState is what Apply compares before it renames.
type fileState struct {
	exists bool
	size   int64
	mod    int64
	ino    uint64
	mode   os.FileMode
}

// Apply reads path, runs edit on its content, and writes the result
// atomically (a temporary file in the same directory, then a rename). A file
// that does not exist is created 0600 in a 0700 directory; an existing
// file keeps its mode. If the file changed between the read and the rename,
// the edit starts over once; a second change returns ErrChanged. This is an
// atomic write, not a lock: a harness that rewrites its file at the same
// instant can still win (plan P6 amendment F6). stat is a seam for tests.
func Apply(path string, edit func([]byte) ([]byte, error), stat func(string) (fileState, error)) (changed bool, err error) {
	if stat == nil {
		stat = statFile
	}
	for attempt := 0; attempt < 2; attempt++ {
		before, err := stat(path)
		if err != nil {
			return false, err
		}
		var content []byte
		if before.exists {
			if content, err = os.ReadFile(path); err != nil {
				return false, err
			}
		}
		out, err := edit(content)
		if err != nil {
			return false, err
		}
		if before.exists && bytes.Equal(out, content) {
			return false, nil
		}
		// Nothing to write to a file that does not exist: disconnect never
		// creates a harness file (review 20261002-d930 F1).
		if !before.exists && len(bytes.TrimSpace(out)) == 0 {
			return false, nil
		}
		mode := os.FileMode(0o600)
		if before.exists {
			mode = before.mode.Perm()
		}
		tmpName, err := writeTemp(path, mode, out)
		if err != nil {
			return false, err
		}
		after, err := stat(path)
		if err != nil || after != before {
			_ = os.Remove(tmpName)
			if err != nil {
				return false, err
			}
			continue // it changed under the edit: start over
		}
		if err := os.Rename(tmpName, path); err != nil {
			_ = os.Remove(tmpName)
			return false, err
		}
		return true, nil
	}
	return false, ErrChanged
}

// writeTemp writes data, synced, to a new temporary file beside path with
// the given mode, creating the directory 0700 if needed. It returns the
// temporary file's name; on error nothing is left behind.
func writeTemp(path string, mode os.FileMode, data []byte) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".vornik-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	err = tmp.Chmod(mode)
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}
