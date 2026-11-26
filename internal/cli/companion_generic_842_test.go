package cli

// GitHub #842 (25-claude-desktop-companion.md §D): a companion path for any
// agent with no Vornik adapter. Found at: e2e-lane (the #783 spike set
// Gemini CLI and opencode up by hand; nothing printed an entry, checked the
// key's repo scope or gave the agent its instructions). Regression test at:
// unit, against a fake companion endpoint in a temp HOME, as the #589 tests.
// Lower because: a real Gemini CLI or opencode cannot run in a test; the
// spike's round trips are the e2e evidence.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testGuidance842 = "# Working alongside Vornik\n\n1. Recall before design work."

// fakeCompanion842 is fakeCompanion with the initialize instructions the
// daemon sends and a chosen effective repo scope.
func fakeCompanion842(t *testing.T, scope string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testCompanionKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "vornik"}, "instructions": testGuidance842}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "recall"}, {"name": "whoami"}}}
		case "tools/call":
			who, _ := json.Marshal(map[string]any{"project_id": "companion-t", "default_repo_scope": scope, "effective_repo_scope": scope})
			if scope == "!badjson" {
				who = []byte("not json")
			}
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(who)}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
}

func genericHome842(t *testing.T, base string) {
	t.Helper()
	desktopHome(t)
	t.Setenv("VORNIK_URL", base)
	companionDeskURL, companionGenericPrint = "", false
	t.Cleanup(func() { companionGenericPrint = false })
}

func TestCompanionConnectGeneric_842_PrintsEntriesNotTheKey(t *testing.T) {
	srv := fakeCompanion842(t, "github.com/o/r")
	defer srv.Close()
	genericHome842(t, srv.URL)
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	companionGenericPrint = true

	var out bytes.Buffer
	if err := runCompanionConnect(context.Background(), "generic", nil, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Contains(s, testCompanionKey) {
		t.Fatal("the key was printed")
	}
	for _, want := range []string{"companion", "mcp-bridge", "--name", "default", srv.URL + "/api/v1/mcp/companion",
		"Bearer ${VORNIK_COMPANION_TOKEN}", "companion-t", "github.com/o/r"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	// The key file is written, 0600, as for Claude Desktop.
	p := filepath.Join(userConfigDir(t), "vornik", "companion", "default.key")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file %s: %v %v", p, st, err)
	}
	// No client config is written: Claude Desktop's file is untouched.
	cfg := filepath.Join(userConfigDir(t), "Claude", "claude_desktop_config.json")
	if b := mustRead(t, cfg); strings.Contains(string(b), "vornik") {
		t.Fatalf("connect generic edited Claude Desktop's config: %s", b)
	}
}

func TestCompanionConnectGeneric_842_WarnsWithoutRepoScope(t *testing.T) {
	srv := fakeCompanion842(t, "")
	defer srv.Close()
	genericHome842(t, srv.URL)
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	companionGenericPrint = true
	var out bytes.Buffer
	if err := runCompanionConnect(context.Background(), "generic", nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--repo-scope") {
		t.Fatalf("no repo-scope warning:\n%s", out.String())
	}
}

func TestCompanionConnectGeneric_842_Refusals(t *testing.T) {
	srv := fakeCompanion842(t, "github.com/o/r")
	defer srv.Close()
	genericHome842(t, srv.URL)
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)

	companionGenericPrint = false
	if err := runCompanionConnect(context.Background(), "generic", nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--print") {
		t.Fatalf("without --print: %v", err)
	}
	companionGenericPrint = true
	t.Setenv("VORNIK_COMPANION_TOKEN", "sk-vornik-wrong")
	if err := runCompanionConnect(context.Background(), "generic", nil, &bytes.Buffer{}); err == nil {
		t.Fatal("a wrong key was accepted")
	}
	if _, err := os.Stat(filepath.Join(userConfigDir(t), "vornik", "companion", "default.key")); err == nil {
		t.Fatal("a refused key was written")
	}
	// --print belongs to generic only.
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	if err := runCompanionConnect(context.Background(), "claude-desktop", nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--print") {
		t.Fatalf("claude-desktop with --print: %v", err)
	}
}

func TestCompanionConnectGeneric_842_RefusesAnAgentKey(t *testing.T) {
	srv := fakeCompanion(t, true)
	defer srv.Close()
	genericHome842(t, srv.URL)
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	companionGenericPrint = true
	if err := runCompanionConnect(context.Background(), "generic", nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "agent connect") {
		t.Fatalf("agent key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userConfigDir(t), "vornik", "companion", "default.key")); err == nil {
		t.Fatal("a refused agent key was written")
	}
}

func TestCompanionInstructions_842(t *testing.T) {
	srv := fakeCompanion842(t, "")
	defer srv.Close()
	genericHome842(t, srv.URL)
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)

	var out bytes.Buffer
	if err := runCompanionInstructions(context.Background(), "", &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{testGuidance842, "recent_memory", "prefix", "repo_scope"} {
		if !strings.Contains(s, want) {
			t.Errorf("instructions lack %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, testCompanionKey) {
		t.Fatal("the key was printed")
	}

	// --write inserts between the markers, keeps the rest, and replaces an
	// earlier copy rather than adding a second.
	f := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(f, []byte("# My project\n\nOur rules.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := runCompanionInstructions(context.Background(), f, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	b := string(mustRead(t, f))
	if !strings.HasPrefix(b, "# My project\n\nOur rules.\n") {
		t.Errorf("the rest of the file moved:\n%s", b)
	}
	if strings.Count(b, "<!-- vornik:start -->") != 1 || strings.Count(b, "<!-- vornik:end -->") != 1 || strings.Count(b, testGuidance842) != 1 {
		t.Errorf("want one copy between the markers:\n%s", b)
	}
}

// With a default repo scope, the repo_scope line is not printed.
func TestCompanionInstructions_842_ScopedKeyNeedsNoScopeLine(t *testing.T) {
	srv := fakeCompanion842(t, "github.com/o/r")
	defer srv.Close()
	genericHome842(t, srv.URL)
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	var out bytes.Buffer
	if err := runCompanionInstructions(context.Background(), "", &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "repo_scope") {
		t.Fatalf("a scoped key got the repo_scope line:\n%s", out.String())
	}
}

// An unreadable whoami is an error, never a false "no repo scope" warning
// (review a004 F3).
func TestCompanionConnectGeneric_842_UnparseableWhoamiFails(t *testing.T) {
	srv := fakeCompanion842(t, "!badjson")
	defer srv.Close()
	genericHome842(t, srv.URL)
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	companionGenericPrint = true
	var out bytes.Buffer
	if err := runCompanionConnect(context.Background(), "generic", nil, &out); err == nil {
		t.Fatalf("an unparseable whoami was accepted:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(userConfigDir(t), "vornik", "companion", "default.key")); err == nil {
		t.Fatal("a key file was written after a failed probe")
	}
}

func TestCompanionInstructions_842_WriteEdges(t *testing.T) {
	srv := fakeCompanion842(t, "github.com/o/r")
	defer srv.Close()
	genericHome842(t, srv.URL)
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	dir := t.TempDir()

	// A missing file is created.
	created := filepath.Join(dir, "new", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(created), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runCompanionInstructions(context.Background(), created, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustRead(t, created)), testGuidance842) {
		t.Fatal("the created file lacks the guidance")
	}

	// A file without a trailing newline keeps its text and gets the section
	// after a blank line; its mode is kept.
	nonl := filepath.Join(dir, "nonl.md")
	if err := os.WriteFile(nonl, []byte("# Rules\nno newline"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCompanionInstructions(context.Background(), nonl, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if b := string(mustRead(t, nonl)); !strings.HasPrefix(b, "# Rules\nno newline\n\n<!-- vornik:start -->\n") {
		t.Errorf("no-newline file became:\n%s", b)
	}
	if st, _ := os.Stat(nonl); st.Mode().Perm() != 0o600 {
		t.Errorf("mode became %v, want 0600", st.Mode().Perm())
	}

	// Unbalanced, doubled or quoted markers are refused, and the file is
	// unchanged (review 76ae F3: a marker quoted in a code block).
	for _, body := range []string{
		"x\n<!-- vornik:start -->\nold\n",
		"<!-- vornik:start -->\na\n<!-- vornik:end -->\n<!-- vornik:start -->\nb\n<!-- vornik:end -->\n",
		"How it works:\n\n```\nVornik writes between `<!-- vornik:start -->` and `<!-- vornik:end -->`.\n```\n",
	} {
		f := filepath.Join(dir, "bad.md")
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runCompanionInstructions(context.Background(), f, &bytes.Buffer{}); err == nil {
			t.Errorf("markers %q were accepted", body)
		}
		if got := string(mustRead(t, f)); got != body {
			t.Errorf("a refused file changed: %q", got)
		}
	}
}

func TestCompanionDisconnectGeneric_842(t *testing.T) {
	srv := fakeCompanion842(t, "github.com/o/r")
	defer srv.Close()
	genericHome842(t, srv.URL)
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	companionGenericPrint = true
	if err := runCompanionConnect(context.Background(), "generic", nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	companionGenericPrint = false
	p := filepath.Join(userConfigDir(t), "vornik", "companion", "default.key")
	if err := runCompanionDisconnect("generic", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the key file survived disconnect generic")
	}
}

func TestCompanionDoctorGeneric_842(t *testing.T) {
	srv := fakeCompanion842(t, "")
	defer srv.Close()
	genericHome842(t, srv.URL)

	// No key file yet: a failure.
	var out bytes.Buffer
	if failed := runCompanionDoctorGeneric(context.Background(), &out); failed == 0 {
		t.Fatalf("no key file passed:\n%s", out.String())
	}

	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	companionGenericPrint = true
	if err := runCompanionConnect(context.Background(), "generic", nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	failed := runCompanionDoctorGeneric(context.Background(), &out)
	s := out.String()
	if failed != 0 {
		t.Fatalf("healthy setup failed %d:\n%s", failed, s)
	}
	for _, want := range []string{"--repo-scope", "VORNIK_COMPANION_TOKEN", "not visible from here"} {
		if !strings.Contains(s, want) {
			t.Errorf("doctor lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Claude Desktop config") {
		t.Errorf("doctor generic checked Claude Desktop:\n%s", s)
	}
	if strings.Contains(s, testCompanionKey) {
		t.Fatal("the key was printed")
	}
}

// disconnect generic with no key file says so and succeeds.
func TestCompanionDisconnectGeneric_842_NothingToRemove(t *testing.T) {
	genericHome842(t, "http://127.0.0.1:1")
	var out bytes.Buffer
	if err := runCompanionDisconnect("generic", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to remove") {
		t.Fatalf("output: %s", out.String())
	}
}

// Daemon instructions that carry a marker are refused before writing
// (review c2f1 N1): written, they would leave a file the next run refuses.
func TestCompanionInstructions_842_RefusesAMarkerInTheText(t *testing.T) {
	f := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := writeInstructionsSection(f, "rules\n<!-- vornik:end -->\n"); err == nil {
		t.Fatal("text with a marker was written")
	}
	if _, err := os.Stat(f); err == nil {
		t.Fatal("a file was created")
	}
}
