package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #581 D5 (enterprise-packaging-design.md, amendment 2026-10-29): an
// owner export carries the config redacted. A managed config holds the
// instance_admin key, the scrape token and the database password literally,
// so each must be gone from the exported copy, in the main file and in the
// configs/ tree.
func TestRedactYAMLFile581_NoCredentialSurvives(t *testing.T) {
	const (
		admin  = "sk-vornik-instance-admin-581-aaaaaaaaaaaaaaaa"
		prev   = "sk-vornik-instance-admin-581-previous-bbbbbbbb"
		scrape = "scrape-581-cccccccccccccccccccccccccc"
		dbPass = "db-password-581-dddddddddddd"
		mcpTok = "ghp_581eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	)
	src := filepath.Join(t.TempDir(), "config.yaml")
	body := `api:
  auth_enabled: true
  api_keys: [` + admin + `, ` + prev + `]
admin:
  enabled: true
  allowed_keys: [` + admin + `, ` + prev + `]
database:
  host: localhost
  password: ` + dbPass + `
metrics:
  scrape_token: ` + scrape + `
hosting:
  mode: managed
  previous_admin_api_key: ` + prev + `
  instance_id: i581
mcp:
  servers:
    - name: gh
      env:
        GITHUB_TOKEN: ` + mcpTok + `
`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "config.yaml")
	if err := redactYAMLFile(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{admin, prev, scrape, dbPass, mcpTok} {
		if strings.Contains(string(got), secret) {
			t.Errorf("the redacted config still holds %q:\n%s", secret, got)
		}
	}
	for _, kept := range []string{"instance_id: i581", "host: localhost", "mode: managed"} {
		if !strings.Contains(string(got), kept) {
			t.Errorf("the redacted config lost %q:\n%s", kept, got)
		}
	}
	if info, err := os.Stat(dst); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("redacted copy mode %v (%v); want 0600", info, err)
	}
}

func TestBackupExport581_RefusesRestorableToo(t *testing.T) {
	backupExport, backupRestorable = true, true
	defer func() { backupExport, backupRestorable = false, false }()
	if err := runBackup(backupCmd, nil); err == nil || !strings.Contains(err.Error(), "--export") {
		t.Errorf("runBackup(--export --restorable) = %v; want a refusal naming both", err)
	}
}

// Commit security review of cef90952f: yaml.v3 decodes a mapping with a
// non-string key into map[any]any, which RedactConfig does not walk, so a
// secret below one passed unredacted. Keys are now made strings first, and
// a type the walk does not know refuses the export.
func TestRedactYAMLFile581_ANonStringKeyHidesNoSecret(t *testing.T) {
	const secret = "sk-vornik-hidden-under-an-int-key-581-ffffffff"
	src := filepath.Join(t.TempDir(), "c.yaml")
	body := "projects:\n  1:\n    password: " + secret + "\n  true:\n    token: " + secret + "\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "c.yaml")
	if err := redactYAMLFile(src, dst); err != nil {
		t.Fatalf("redactYAMLFile = %v; int and bool keys are ordinary YAML and must redact, not refuse", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), secret) {
		t.Errorf("a secret below a non-string key survived the redaction:\n%s", got)
	}

	// A key that cannot be made a string (a sequence) refuses the file, so
	// nothing reaches the export.
	if err := os.WriteFile(src, []byte("? [a, b]\n: {api_key: "+secret+"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := redactYAMLFile(src, dst+".2"); err == nil {
		if got, _ := os.ReadFile(dst + ".2"); strings.Contains(string(got), secret) {
			t.Errorf("a secret below a sequence key reached the export:\n%s", got)
		}
	}
}

// Commit security review of bab44b9b2: the redaction is by key name, so a
// token in a URL or under a neutral key passed. String leaves are now also
// checked by value.
func TestRedactYAMLFile581_RedactsByValueToo(t *testing.T) {
	const (
		ghp     = "ghp_0123456789abcdefghijABCDEFGHIJ012345"
		urlUser = "ghp_userinfo0123456789abcdefghijABCDEF"
		urlQry  = "qrysecret-0123456789abcdef"
	)
	src := filepath.Join(t.TempDir(), "c.yaml")
	body := `mcp:
  servers:
    - name: gh
      homepage: https://x-access:` + urlUser + `@mcp.example.com/sse?token=` + urlQry + `
      args: ["--flag", "` + ghp + `"]
chat:
  endpoint: https://gateway.example.com/v1
description: a long human sentence that is not a secret at all, kept as written
`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "c.yaml")
	if err := redactYAMLFile(src, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	for _, secret := range []string{ghp, urlUser, urlQry} {
		if strings.Contains(string(got), secret) {
			t.Errorf("%q survived the value redaction:\n%s", secret, got)
		}
	}
	for _, kept := range []string{"https://gateway.example.com/v1", "mcp.example.com/sse", "a long human sentence that is not a secret at all, kept as written", "--flag"} {
		if !strings.Contains(string(got), kept) {
			t.Errorf("the value redaction lost %q:\n%s", kept, got)
		}
	}
}

// Commit security review of bab44b9b2: workspaces were exported whole, so a
// git remote's token or a credentials file went with them.
func TestScrubWorkspaceCredentials581(t *testing.T) {
	ws := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const tok = "ghp_remote0123456789abcdefghijABCDEFGHIJ"
	write("p1/.git/config", "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = https://x-access-token:"+tok+"@github.com/acme/repo.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n")
	write("p1/.git-credentials", "https://u:"+tok+"@github.com\n")
	write("p1/.env", "TOKEN="+tok+"\n")
	write("p1/sub/.env.local", "X=1\n")
	write("p1/.netrc", "machine github.com password "+tok+"\n")
	write("p1/README.md", "keep me\n")
	write("p1/src/env.go", "package env\n")

	removed, rewritten, err := scrubWorkspaceCredentials(ws)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 4 || rewritten != 1 {
		t.Errorf("removed %d files and rewrote %d git configs; want 4 and 1", removed, rewritten)
	}
	for _, gone := range []string{"p1/.git-credentials", "p1/.env", "p1/sub/.env.local", "p1/.netrc"} {
		if _, err := os.Stat(filepath.Join(ws, gone)); err == nil {
			t.Errorf("%s is still in the export", gone)
		}
	}
	cfg, _ := os.ReadFile(filepath.Join(ws, "p1/.git/config"))
	if strings.Contains(string(cfg), tok) || !strings.Contains(string(cfg), "https://github.com/acme/repo.git") {
		t.Errorf("the remote kept its token or lost its URL:\n%s", cfg)
	}
	for _, kept := range []string{"p1/README.md", "p1/src/env.go"} {
		if _, err := os.Stat(filepath.Join(ws, kept)); err != nil {
			t.Errorf("%s was removed", kept)
		}
	}
}

// Background security review of the scrub: pushurl, an extraheader (how
// actions/checkout stores its token), a [credential] section, a submodule's
// config under .git/modules, and a userinfo holding an "@" all carried a
// token past the first version.
func TestScrubWorkspaceCredentials581_EveryGitConfigShape(t *testing.T) {
	ws := t.TempDir()
	const tok = "ghp_modules0123456789abcdefghijABCDEFGHI"
	cfg := "[remote \"origin\"]\n\tURL = https://user:p@ss" + tok + "@github.com/a/b.git\n\tpushurl = https://x:" + tok + "@github.com/a/b.git\n" +
		"[http \"https://github.com/\"]\n\textraheader = AUTHORIZATION: basic " + tok + "\n" +
		"[credential]\n\thelper = store\n\tusername = bot\n[core]\n\tbare = false\n"
	for _, rel := range []string{"p/.git/config", "p/.git/modules/sub/config"} {
		path := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, rewritten, err := scrubWorkspaceCredentials(ws); err != nil || rewritten != 2 {
		t.Fatalf("rewritten %d, err %v; want both git configs", rewritten, err)
	}
	for _, rel := range []string{"p/.git/config", "p/.git/modules/sub/config"} {
		got, _ := os.ReadFile(filepath.Join(ws, rel))
		for _, bad := range []string{tok, "p@ss", "extraheader", "[credential]", "helper = store"} {
			if strings.Contains(string(got), bad) {
				t.Errorf("%s still holds %q:\n%s", rel, bad, got)
			}
		}
		for _, kept := range []string{"https://github.com/a/b.git", "bare = false"} {
			if !strings.Contains(string(got), kept) {
				t.Errorf("%s lost %q:\n%s", rel, kept, got)
			}
		}
	}
}

// Commit security review of 41e07408b: the scrub was a set of denylists (file
// names, git config keys, whole-value URLs), and each review found a shape
// outside them. The export now scrubs every text file of the workspace copy:
// a URL's userinfo anywhere on any line, and every strong-pattern match.
func TestScrubWorkspaceCredentials581_EveryTextFileIsScrubbed(t *testing.T) {
	ws := t.TempDir()
	const tok = "ghp_anywhere0123456789abcdefghijABCDEFGH"
	files := map[string]string{
		"bare.git/config":        "[remote \"o\"]\n\turl = https://u:" + tok + "@host.example/r.git\n",
		"p/.git/config.worktree": "[credential.https://host.example]\n\tusername = bot\n\thelper = store\n[core]\n\tbare = false\n",
		"p/.git/config":          "[url \"https://bot:" + tok + "@host.example/\"]\n\tinsteadOf = https://host.example/\n",
		"p/.gitmodules":          "[submodule \"s\"]\n\turl = https://u:" + tok + "@host.example/s.git\n",
		"p/docs/setup.md":        "Clone with git clone \"https://bot:" + tok + "@host.example/r.git\" and the key " + tok + ".\n",
		"p/src/main.go":          "package main\n\nconst endpoint = \"https://host.example/api\"\n",
	}
	for rel, body := range files {
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(ws, "p/.git/objects/pack/x.pack")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, append([]byte{0, 1, 2}, []byte(tok)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scrubWorkspaceCredentials(ws); err != nil {
		t.Fatal(err)
	}
	for rel := range files {
		got, _ := os.ReadFile(filepath.Join(ws, rel))
		for _, bad := range []string{tok, "bot:", "u:", "helper = store", "[credential"} {
			if strings.Contains(string(got), bad) {
				t.Errorf("%s still holds %q:\n%s", rel, bad, got)
			}
		}
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "p/src/main.go")); !strings.Contains(string(got), `"https://host.example/api"`) {
		t.Errorf("a plain URL was mangled:\n%s", got)
	}
	if got, _ := os.ReadFile(binary); !strings.Contains(string(got), tok) {
		t.Error("a binary file was rewritten; it is copied as it is")
	}
}

// The config value pass finds a URL anywhere in a string, not only when the
// whole value is one (the same review).
func TestRedactYAMLFile581_AURLInsideAValue(t *testing.T) {
	const tok = "inline-secret-0123456789abcdef"
	src := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(src, []byte("note: \"fetch from https://bot:"+tok+"@h.example/x then run\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "c.yaml")
	if err := redactYAMLFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); strings.Contains(string(got), tok) || !strings.Contains(string(got), "h.example/x then run") {
		t.Errorf("the inline URL kept its userinfo, or the text was lost:\n%s", got)
	}
}

// Commit security review of 41e07408b: owner-data tables keep task results
// and workflow definitions, where a token can be written. The export's dump
// is scrubbed by value, line by line, as the workspaces are.
func TestScrubDumpFile581_RedactsByValueAndKeepsTheFormat(t *testing.T) {
	const tok = "ghp_dumped0123456789abcdefghijABCDEFGHIJ"
	dump := filepath.Join(t.TempDir(), "db.sql")
	body := "COPY public.tasks (id, result_envelope) FROM stdin;\n" +
		"t1\t{\"out\": \"pushed with https://bot:" + tok + "@host.example/r.git\"}\n" +
		"t2\t{\"out\": \"the key is " + tok + "\"}\n" +
		"t3\t{\"out\": \"nothing here\"}\n\\.\n"
	if err := os.WriteFile(dump, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := scrubDumpFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dump)
	if n != 2 || strings.Contains(string(got), tok) || strings.Contains(string(got), "bot:") {
		t.Errorf("scrubbed %d lines; want 2 and no secret left:\n%s", n, got)
	}
	if lines := strings.Count(string(got), "\n"); lines != strings.Count(body, "\n") {
		t.Errorf("the dump has %d lines, was %d: the COPY format must survive", lines, strings.Count(body, "\n"))
	}
	if !strings.Contains(string(got), "t3\t{\"out\": \"nothing here\"}") || !strings.Contains(string(got), "\t") {
		t.Errorf("a clean row changed, or the tabs were lost:\n%s", got)
	}
}
