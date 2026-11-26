package cli

// GitHub #841, LLD 29 B7: `vornikctl companion connect|doctor|disconnect
// opencode` against a fake companion endpoint in a temp HOME. A real opencode
// run is the design's live check, recorded in LLD 29.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func opencodeHome(t *testing.T) (home, dir string) {
	t.Helper()
	home, _ = desktopHome(t)
	companionAllowUnscoped = false
	return home, filepath.Join(home, ".config", "opencode")
}

func TestCompanionConnectOpenCode_841_InstallsTheBundle(t *testing.T) {
	home, dir := opencodeHome(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The user's own config and skill survive.
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(`{"model":"x/y","mcp":{"other":{"type":"remote","url":"https://o"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills", "my-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := fakeScopedCompanion(t, "github.com/o/r")
	defer srv.Close()
	companionDeskURL = srv.URL
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	var out bytes.Buffer
	if err := runCompanionConnect(context.Background(), "opencode", nil, &out); err != nil {
		t.Fatalf("connect opencode: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Model string                    `json:"model"`
		MCP   map[string]map[string]any `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	e := cfg.MCP["vornik-companion-default"]
	cmd, _ := e["command"].([]any)
	if e["type"] != "local" || len(cmd) == 0 || !filepath.IsAbs(cmd[0].(string)) || cfg.MCP["other"] == nil || cfg.Model != "x/y" {
		t.Fatalf("opencode.json after connect: %s", raw)
	}

	plugin, err := os.ReadFile(filepath.Join(dir, "plugin", "vornik-companion.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"const VORNIKCTL = \"" + cmd[0].(string) + "\";", `const KEY_NAME = "default";`, `const VORNIK_URL = "` + srv.URL + `";`} {
		if !strings.Contains(string(plugin), want) {
			t.Errorf("installed plugin lacks %q", want)
		}
	}
	for _, f := range []string{"plugin/vornik-companion-guidance.md", "skills/vornik-delegate/SKILL.md", "vornik-companion/scripts/vornik-backlog-deposit.sh"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not installed: %v", f, err)
		}
	}
	if strings.Contains(out.String(), testCompanionKey) {
		t.Fatal("connect printed the key")
	}
	keyPath := filepath.Join(userConfigDir(t), "vornik", "companion", "default.key")
	_ = filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || p == keyPath {
			return nil
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(testCompanionKey)) {
			t.Errorf("%s holds the key; only the key file may", p)
		}
		return nil
	})

	var doc bytes.Buffer
	if failed := runCompanionDoctorOpenCode(context.Background(), &doc); failed != 0 {
		t.Fatalf("doctor after connect: %d failure(s)\n%s", failed, doc.String())
	}

	out.Reset()
	if err := runCompanionDisconnect("opencode", &out); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	// Found in the 2026-11-21 live check: disconnect removed the entry, then
	// told the user to remove it themselves.
	if strings.Contains(out.String(), "yourself") {
		t.Errorf("disconnect removed the entry but tells the user to remove it:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "plugin")); !os.IsNotExist(err) {
		t.Error("disconnect left the plugin directory connect created, empty")
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "opencode.json"))
	if strings.Contains(string(raw), "vornik-companion-default") || !strings.Contains(string(raw), `"other"`) {
		t.Fatalf("disconnect must remove only Vornik's entry: %s", raw)
	}
	for _, p := range []string{"plugin/vornik-companion.js", "plugin/vornik-companion-guidance.md", "skills/vornik-delegate", "vornik-companion"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("%s survives disconnect", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", "my-skill")); err != nil {
		t.Fatal("disconnect removed the user's own skill")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("disconnect left the key file")
	}
}

func TestCompanionConnectOpenCode_841_RefusalsWriteNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		key, scope, config string
		foreignPlugin      bool
	}{
		"wrong key":        {key: "sk-vornik-companion-x.wrong", scope: "github.com/o/r"},
		"unscoped key":     {key: testCompanionKey},
		"commented config": {key: testCompanionKey, scope: "github.com/o/r", config: "{\n  // mine\n  \"model\": \"x\"\n}"},
		"a foreign plugin": {key: testCompanionKey, scope: "github.com/o/r", foreignPlugin: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, dir := opencodeHome(t)
			if tc.config != "" {
				_ = os.MkdirAll(dir, 0o755)
				_ = os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(tc.config), 0o600)
			}
			if tc.foreignPlugin {
				_ = os.MkdirAll(filepath.Join(dir, "plugin"), 0o755)
				_ = os.WriteFile(filepath.Join(dir, "plugin", "vornik-companion.js"), []byte("// someone else's plugin\n"), 0o600)
			}
			srv := fakeScopedCompanion(t, tc.scope)
			defer srv.Close()
			companionDeskURL = srv.URL
			t.Setenv("VORNIK_COMPANION_TOKEN", tc.key)
			var out bytes.Buffer
			if err := runCompanionConnect(context.Background(), "opencode", nil, &out); err == nil {
				t.Fatal("connect must refuse")
			}
			if _, err := os.Stat(filepath.Join(dir, "skills")); !os.IsNotExist(err) {
				t.Error("a refusal installed skills")
			}
			if tc.foreignPlugin {
				if b, _ := os.ReadFile(filepath.Join(dir, "plugin", "vornik-companion.js")); string(b) != "// someone else's plugin\n" {
					t.Error("a foreign plugin was overwritten")
				}
			}
			if tc.config != "" {
				if b, _ := os.ReadFile(filepath.Join(dir, "opencode.json")); string(b) != tc.config {
					t.Error("a commented config was rewritten")
				}
			}
		})
	}
}
