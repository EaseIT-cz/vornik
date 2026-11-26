package harnessconfig

// GitHub #841, LLD 29 B2: opencode is the fifth harness whose config the CLI
// edits. Its opencode.json takes an "mcp" map of {"type": "local", "command":
// [argv...]}; every other key and entry is kept, and a file that is not plain
// JSON (comments) is refused, never rewritten.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenCode_841_Path(t *testing.T) {
	env := Env{Home: "/h", ConfigDir: "/h/.config", Getenv: func(k string) string {
		return map[string]string{"XDG_CONFIG_HOME": "/xdg"}[k]
	}}
	if got, err := Path(OpenCode, env); err != nil || got != "/xdg/opencode/opencode.json" {
		t.Fatalf("with XDG_CONFIG_HOME: %q %v", got, err)
	}
	env.Getenv = func(string) string { return "" }
	if got, err := Path(OpenCode, env); err != nil || got != "/h/.config/opencode/opencode.json" {
		t.Fatalf("default: %q %v", got, err)
	}
}

func TestOpenCode_841_SetRemove(t *testing.T) {
	companion := Entry{Kind: KindCompanion, Namespace: "default", Command: "/usr/bin/vornikctl", URL: "https://v.example"}
	in := []byte(`{"$schema": "https://opencode.ai/config.json", "model": "x/y", "mcp": {"other": {"type": "remote", "url": "https://o"}}}`)
	out, err := Set(OpenCode, in, companion)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	mcp := m["mcp"].(map[string]any)
	got := mcp["vornik-companion-default"].(map[string]any)
	if got["type"] != "local" || strings.Join(toStrings(got["command"]), " ") != "/usr/bin/vornikctl companion mcp-bridge --name default --url https://v.example" {
		t.Fatalf("entry: %+v", got)
	}
	if mcp["other"] == nil || m["model"] != "x/y" || m["$schema"] == nil {
		t.Fatalf("lost content: %s", out)
	}
	if _, has := m["mcpServers"]; has {
		t.Fatal("opencode's servers live under mcp, not mcpServers")
	}
	again, _ := Set(OpenCode, out, companion)
	if string(again) != string(out) {
		t.Fatal("not idempotent")
	}
	removed, ok, err := Remove(OpenCode, out, Entry{Kind: KindCompanion, Namespace: "default"})
	if err != nil || !ok || strings.Contains(string(removed), "vornik-companion-default") || !strings.Contains(string(removed), `"other"`) {
		t.Fatalf("remove: %v %v %s", ok, err, removed)
	}
	if d, err := Set(OpenCode, nil, companion); err != nil || !strings.Contains(string(d), "vornik-companion-default") {
		t.Fatalf("an empty file starts an object: %v %s", err, d)
	}
	for _, bad := range []string{"{\n  // my settings\n  \"model\": \"x/y\"\n}", `{"mcp": []}`, `not json`} {
		if _, err := Set(OpenCode, []byte(bad), companion); err == nil {
			t.Errorf("%q was rewritten", bad)
		}
	}
}
