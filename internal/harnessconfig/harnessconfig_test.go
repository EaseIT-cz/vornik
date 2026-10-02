package harnessconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const bridge = "/usr/local/bin/vornikctl"

var entry = Entry{Namespace: "hermes", Command: bridge}

// Plan P6.2: each harness's file is where that harness reads its MCP
// servers, overridable by the harness's own home variable. Control: Path.
func TestPath(t *testing.T) {
	env := Env{Home: "/h", ConfigDir: "/h/.config", Getenv: func(k string) string {
		return map[string]string{"HERMES_HOME": "/srv/hermes"}[k]
	}}
	for h, want := range map[string]string{
		ClaudeCode: "/h/.claude.json", ClaudeDesktop: "/h/.config/Claude/claude_desktop_config.json",
		Codex: "/h/.codex/config.toml", Hermes: "/srv/hermes/config.yaml",
	} {
		if got, err := Path(h, env); err != nil || got != want {
			t.Errorf("%s: %q %v, want %s", h, got, err, want)
		}
	}
	if _, err := Path("cursor", env); err == nil {
		t.Error("an unknown harness had a path")
	}
}

// Every format: the entry runs the bridge for the namespace, another entry
// and the rest of the file survive, the edit is idempotent, and Remove
// takes out exactly the entry. The key appears nowhere, because Entry has
// no field for it. Control: Set and Remove per format.
func TestSetRemove_JSON(t *testing.T) {
	in := []byte(`{"numStartups": 12345678901234567890, "mcpServers": {"other": {"command": "x"}}, "projects": {"/a": {"allowedTools": []}}}`)
	out, err := Set(ClaudeCode, in, entry)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	srv := m["mcpServers"].(map[string]any)
	got := srv["vornik-hermes"].(map[string]any)
	if got["command"] != bridge || got["type"] != "stdio" || strings.Join(toStrings(got["args"]), " ") != "agent mcp-bridge --namespace hermes" {
		t.Fatalf("entry: %+v", got)
	}
	if srv["other"] == nil || m["projects"] == nil || !strings.Contains(string(out), "12345678901234567890") {
		t.Fatalf("lost content: %s", out)
	}
	again, _ := Set(ClaudeCode, out, entry)
	if string(again) != string(out) {
		t.Fatal("not idempotent")
	}
	removed, ok, err := Remove(ClaudeCode, out, "hermes")
	if err != nil || !ok || strings.Contains(string(removed), "vornik-hermes") || !strings.Contains(string(removed), `"other"`) {
		t.Fatalf("remove: %v %v %s", ok, err, removed)
	}
	// Claude Desktop takes no type field; an empty file starts an object.
	d, err := Set(ClaudeDesktop, nil, entry)
	if err != nil || strings.Contains(string(d), `"type"`) || !strings.Contains(string(d), "vornik-hermes") {
		t.Fatalf("desktop: %v %s", err, d)
	}
	if _, err := Set(ClaudeCode, []byte(`{"mcpServers": []}`), entry); err == nil {
		t.Fatal("a non-object mcpServers was overwritten")
	}
	// A file that does not parse is refused, never replaced (review
	// 20261002-00bc F3).
	for _, bad := range []string{`{"a": 1,}`, `not json`, `[1, 2]`} {
		if _, err := Set(ClaudeDesktop, []byte(bad), entry); err == nil {
			t.Errorf("%q was overwritten", bad)
		}
		if _, _, err := Remove(ClaudeDesktop, []byte(bad), "hermes"); err == nil {
			t.Errorf("%q: remove did not refuse", bad)
		}
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// Hermes YAML: comments survive (plan P6 amendment F5).
func TestSetRemove_YAML(t *testing.T) {
	in := []byte("# my hermes\nmodel: gpt # keep me\nmcp_servers:\n  # the files server\n  files:\n    command: fs\n")
	out, err := Set(Hermes, in, entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"# my hermes", "# keep me", "# the files server", "files:"} {
		if !strings.Contains(string(out), c) {
			t.Errorf("lost %q:\n%s", c, out)
		}
	}
	var m struct {
		MCP map[string]struct {
			Command string   `yaml:"command"`
			Args    []string `yaml:"args"`
		} `yaml:"mcp_servers"`
	}
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if e := m.MCP["vornik-hermes"]; e.Command != bridge || strings.Join(e.Args, " ") != "agent mcp-bridge --namespace hermes" {
		t.Fatalf("entry: %+v", e)
	}
	again, _ := Set(Hermes, out, entry)
	if string(again) != string(out) {
		t.Fatalf("not idempotent:\n%s\n---\n%s", out, again)
	}
	removed, ok, err := Remove(Hermes, out, "hermes")
	if err != nil || !ok || strings.Contains(string(removed), "vornik-hermes") || !strings.Contains(string(removed), "# the files server") {
		t.Fatalf("remove: %v %v\n%s", ok, err, removed)
	}
	if fresh, err := Set(Hermes, nil, entry); err != nil || !strings.Contains(string(fresh), "vornik-hermes") {
		t.Fatalf("empty file: %v %s", err, fresh)
	}
}

// Codex TOML: every byte outside the block is kept, nested tables of the
// block are replaced with it, and forms a text edit cannot handle are
// refused (review 6f6b F7).
func TestSetRemove_TOML(t *testing.T) {
	in := "# codex settings\nmodel = \"o4\" # keep\n\n[mcp_servers.files]\ncommand = \"fs\"\n\n# trailing comment\n[profiles.x]\na = 1\n"
	out, err := Set(Codex, []byte(in), entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), in) {
		t.Fatalf("the file before the block changed:\n%s", out)
	}
	if !strings.Contains(string(out), "[mcp_servers.vornik-hermes]\ncommand = \""+bridge+"\"\nargs = [\"agent\", \"mcp-bridge\", \"--namespace\", \"hermes\"]\n") {
		t.Fatalf("block:\n%s", out)
	}
	again, _ := Set(Codex, out, entry)
	if string(again) != string(out) {
		t.Fatalf("not idempotent:\n%s\n---\n%s", out, again)
	}
	removed, ok, err := Remove(Codex, out, "hermes")
	if err != nil || !ok || string(removed) != in {
		t.Fatalf("remove did not restore the file: %v %v\n%q\n%q", ok, err, removed, in)
	}

	// A block in the middle, with a table under it and a comment that
	// belongs to the next table: replaced in place, the rest kept.
	mid := "[mcp_servers.vornik-hermes]\ncommand = \"old\"\n[mcp_servers.vornik-hermes.env]\nX = \"1\"\n\n# profiles\n[profiles.x]\na = 1\n"
	out, err = Set(Codex, []byte(mid), entry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "old") || strings.Contains(string(out), ".env]") || !strings.HasSuffix(string(out), "\n# profiles\n[profiles.x]\na = 1\n") {
		t.Fatalf("in-place replace:\n%s", out)
	}

	for name, bad := range map[string]struct{ in, says string }{
		"array of tables": {"[[mcp_servers.vornik-hermes]]\ncommand = \"x\"\n", "array of tables"},
		"inline table":    {"[mcp_servers]\nvornik-hermes = { command = \"x\" }\n", "in a form this tool does not edit"},
		"dotted key":      {"mcp_servers.vornik-hermes.command = \"x\"\n", "in a form this tool does not edit"},
		"split":           {"[mcp_servers.vornik-hermes]\na = 1\n[other]\n[mcp_servers.vornik-hermes.env]\n", "split across"},
	} {
		if _, err := Set(Codex, []byte(bad.in), entry); err == nil || !strings.Contains(err.Error(), bad.says) {
			t.Errorf("%s: %v, want a refusal saying %q", name, err, bad.says)
		}
	}
	// Disconnect removes the block with the tables under it, and refuses
	// what connect refuses (review 20261002-00bc F1).
	if out, ok, err := Remove(Codex, []byte(mid), "hermes"); err != nil || !ok || string(out) != "# profiles\n[profiles.x]\na = 1\n" {
		t.Fatalf("remove with a nested table: %v %v\n%q", ok, err, out)
	}
	if _, _, err := Remove(Codex, []byte("[[mcp_servers.vornik-hermes]]\n"), "hermes"); err == nil || !strings.Contains(err.Error(), "array of tables") {
		t.Fatalf("remove an array of tables: %v", err)
	}
	// Quoted and spaced headers are the same table.
	q := "[ mcp_servers . \"vornik-hermes\" ]\ncommand = \"old\"\n"
	if out, err := Set(Codex, []byte(q), entry); err != nil || strings.Contains(string(out), "old") {
		t.Fatalf("quoted header: %v\n%s", err, out)
	}
}

// A command with a quote or a control character stays one TOML string.
func TestTOMLString(t *testing.T) {
	if got := tomlString("a\"b\\c\nd"); got != `"a\"b\\c\u000Ad"` {
		t.Fatalf("%s", got)
	}
}

// An entry is refused for a namespace that is not one, or a relative
// command (the harness would resolve it against its own PATH).
func TestEntryChecked(t *testing.T) {
	for _, e := range []Entry{{Namespace: "../x", Command: bridge}, {Namespace: "hermes", Command: "vornikctl"},
		{Namespace: "hermes", Command: bridge, URL: "http://192.0.2.1:8080"}} {
		if _, err := Set(Hermes, nil, e); err == nil {
			t.Errorf("%+v accepted", e)
		}
	}
}

// Apply: a new file is 0600 in a 0700 directory; an existing file keeps its
// mode; a file that changes between read and rename makes the edit start
// over once, and a second change refuses (plan P6 amendment F6). Control:
// Apply's re-stat.
func TestApply(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	p := filepath.Join(dir, "config.yaml")
	edit := func(b []byte) ([]byte, error) { return Set(Hermes, b, entry) }
	if changed, err := Apply(p, edit, nil); err != nil || !changed {
		t.Fatalf("create: %v %v", changed, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("created %v", fi.Mode())
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir %v", fi.Mode())
	}
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if changed, err := Apply(p, func(b []byte) ([]byte, error) { return append(b, "# x\n"...), nil }, nil); err != nil || !changed {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode not kept: %v", fi.Mode())
	}
	if changed, _ := Apply(p, func(b []byte) ([]byte, error) { return b, nil }, nil); changed {
		t.Fatal("an unchanged file was rewritten")
	}

	// The harness rewrites the file once mid-edit: the edit starts over.
	calls := 0
	racy := func(path string) (fileState, error) {
		calls++
		st, err := statFile(path)
		if calls == 2 {
			st.size++ // a change between the read and the rename
		}
		return st, err
	}
	if changed, err := Apply(p, func(b []byte) ([]byte, error) { return append(b, "# y\n"...), nil }, racy); err != nil || !changed || calls != 4 {
		t.Fatalf("one change: %v %v after %d stats", changed, err, calls)
	}
	// It keeps changing: refused, and the file is untouched.
	before, _ := os.ReadFile(p)
	always := func(path string) (fileState, error) {
		calls++
		st, err := statFile(path)
		st.mod += int64(calls)
		return st, err
	}
	if _, err := Apply(p, func(b []byte) ([]byte, error) { return append(b, "# z\n"...), nil }, always); !errors.Is(err, ErrChanged) {
		t.Fatalf("two changes: %v", err)
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Fatal("the file was written although it kept changing")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".config.yaml.vornik-*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	// A symlink is refused, not replaced.
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(link, edit, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked config: %v", err)
	}
}

// Review 20261002-d930 F1: an edit that leaves an absent file empty writes
// nothing, so disconnect never creates a harness file. Control: Apply's
// no-op guard for a file that does not exist.
func TestApply_AbsentAndEmptyWritesNothing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "absent.yaml")
	changed, err := Apply(p, func(b []byte) ([]byte, error) {
		out, _, err := Remove(Hermes, b, "hermes")
		return out, err
	}, nil)
	if err != nil || changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("an absent harness file was created")
	}
}

// Review 20261002-d930 F2: the name elsewhere in the file counts only as a
// whole key, outside comments; a comment that mentions it, or a sibling
// whose name extends it, is not a definition.
func TestSetTOML_NameElsewhere(t *testing.T) {
	for name, in := range map[string]string{
		"comment":        "# vornik-hermes is managed by vornikctl\nmodel = \"o4\"\n",
		"longer sibling": "[mcp_servers.vornik-hermes2]\ncommand = \"x\"\n",
		"in a value":     "note = \"see vornik-hermes docs\"\n",
	} {
		out, err := Set(Codex, []byte(in), entry)
		if err != nil {
			t.Errorf("%s: refused: %v", name, err)
			continue
		}
		if !strings.HasPrefix(string(out), in) {
			t.Errorf("%s: the rest of the file changed:\n%s", name, out)
		}
	}
}

// Regression (DoD lane bring-up, 2026-10-02): the entry a harness runs must
// carry the daemon's URL, because a harness starts the bridge with its own
// environment, where VORNIK_API_URL is not set; without it the bridge
// dialled the default and a daemon elsewhere was unreachable. Control:
// Entry.Args with URL.
func TestEntryArgsCarryURL(t *testing.T) {
	e := Entry{Namespace: "hermes", Command: bridge, URL: "https://vornik.example:8443"}
	if got := strings.Join(e.Args(), " "); got != "agent mcp-bridge --namespace hermes --url https://vornik.example:8443" {
		t.Fatalf("args: %s", got)
	}
	out, err := Set(Codex, nil, e)
	if err != nil || !strings.Contains(string(out), `"--url", "https://vornik.example:8443"`) {
		t.Fatalf("codex block: %v\n%s", err, out)
	}
	if got := strings.Join(entry.Args(), " "); got != "agent mcp-bridge --namespace hermes" {
		t.Fatalf("no URL: %s", got)
	}
}
