package spawn

// Process-spawn law, S6 (https://docs.vornik.io,
// "S6 — git state an agent can write"): git runs programs chosen by the state
// it reads. D3 pins WHERE a daemon git command in a task worktree may read its
// repository from (the admin dir the daemon names, never the agent-writable
// .git pointer); D4 pins which global config it may read (a composed file
// holding only allowlisted keys, never the system config).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// worktreeFixture lays out <root>/<proj>/.worktrees/<name> and its admin dir
// under a registered root, without running git (D3 is judged on paths).
func worktreeFixture(t *testing.T) (root, proj, wt, admin string) {
	t.Helper()
	root = withRoot(t)
	proj = filepath.Join(root, "p1")
	wt = filepath.Join(proj, ".worktrees", "task_1")
	admin = filepath.Join(proj, ".git", "worktrees", "task_1")
	for _, d := range []string{wt, admin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, proj, wt, admin
}

func envValue(env []string, key string) (string, bool) {
	v := ""
	found := false
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v, found = val, true // last one wins, as in the process
		}
	}
	return v, found
}

// S6-D3: a worktree command must carry exactly the daemon-named GIT_DIR and
// GIT_WORK_TREE. Missing or wrong, either one, is refused before git starts.
func TestS6_D3_WorktreeCommandMustNameItsGitDirs(t *testing.T) {
	ctx := context.Background()
	_, proj, wt, admin := worktreeFixture(t)

	c := must(t, "both pinned")(GitWorkspaceDirs(ctx, wt, GitDirs{GitDir: admin, WorkTree: wt}, "status", "--porcelain"))
	env := c.Env()
	if v, _ := envValue(env, "GIT_DIR"); v != admin {
		t.Fatalf("GIT_DIR = %q, want %q", v, admin)
	}
	if v, _ := envValue(env, "GIT_WORK_TREE"); v != wt {
		t.Fatalf("GIT_WORK_TREE = %q, want %q", v, wt)
	}
	// A subdirectory of the worktree is the worktree too.
	sub := filepath.Join(wt, "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, "a subdirectory, pinned")(GitWorkspaceDirs(ctx, sub, GitDirs{GitDir: admin, WorkTree: wt}, "status"))

	other := filepath.Join(proj, ".git", "worktrees", "task_2")
	for name, dirs := range map[string]GitDirs{
		"missing both":          {},
		"missing GIT_WORK_TREE": {GitDir: admin},
		"missing GIT_DIR":       {WorkTree: wt},
		"wrong GIT_DIR":         {GitDir: other, WorkTree: wt},
		"GIT_DIR = the project": {GitDir: filepath.Join(proj, ".git"), WorkTree: wt},
		"wrong GIT_WORK_TREE":   {GitDir: admin, WorkTree: proj},
	} {
		c, err := GitWorkspaceDirs(ctx, wt, dirs, "status")
		refused(t, c, err, "via -C: "+name)
		c, err = GitWorkspaceDirs(ctx, sub, dirs, "status")
		refused(t, c, err, "via a subdirectory: "+name)
	}
	c2, err := GitWorkspace(ctx, wt, "status")
	refused(t, c2, err, "GitWorkspace (no dirs) in a worktree")
}

// The command's directory can reach git only as -C: the git kinds take no
// working-directory option, and a -C in the arguments is not a subcommand.
// So "via Dir" (design S6-D3) has one path to check, and it is -C.
func TestS6_D3_TheDirectoryIsOnlyEverMinusC(t *testing.T) {
	ctx := context.Background()
	root, _, wt, admin := worktreeFixture(t)
	c := must(t, "project")(GitWorkspace(ctx, root, "status"))
	if c.Dir() != "" {
		t.Fatalf("a git command must not carry a working directory, got %q", c.Dir())
	}
	c2, err := GitWorkspace(ctx, root, "-C", wt, "status")
	refused(t, c2, err, "a second -C in the arguments")
	c2, err = GitWorkspaceDirs(ctx, root, GitDirs{GitDir: admin, WorkTree: wt}, "status")
	refused(t, c2, err, "git dirs on a command whose directory is not that worktree")
}

// Outside a worktree, GIT_DIR/GIT_WORK_TREE are never settable, and an
// inherited daemon value never reaches git.
func TestS6_D3_ProjectCommandsCarryNoGitDirs(t *testing.T) {
	ctx := context.Background()
	root := withRoot(t)
	t.Setenv("GIT_DIR", "/etc")
	t.Setenv("GIT_WORK_TREE", "/")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor'='/tmp/x'")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	c := must(t, "project")(GitWorkspace(ctx, root, "status"))
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT"} {
		if v, ok := envValue(c.Env(), k); ok {
			t.Errorf("inherited %s=%q reached a project git command", k, v)
		}
	}
}

// S6-D4 scope: every daemon git command, in the project, in a worktree and
// git http-backend, reads no system config and the composed global config.
func TestS6_D4_EveryGitKindReadsOnlyTheComposedConfig(t *testing.T) {
	ctx := context.Background()
	root, _, wt, admin := worktreeFixture(t)
	composed := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(composed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	restore := SetGitGlobalConfigForTest(composed)
	defer restore()

	check := func(name string, env []string) {
		t.Helper()
		if v, _ := envValue(env, "GIT_CONFIG_NOSYSTEM"); v != "1" {
			t.Errorf("%s: GIT_CONFIG_NOSYSTEM = %q", name, v)
		}
		if v, _ := envValue(env, "GIT_CONFIG_GLOBAL"); v != composed {
			t.Errorf("%s: GIT_CONFIG_GLOBAL = %q, want %q", name, v, composed)
		}
	}
	check("project", must(t, "project")(GitWorkspace(ctx, root, "status")).Env())
	check("worktree", must(t, "worktree")(GitWorkspaceDirs(ctx, wt, GitDirs{GitDir: admin, WorkTree: wt}, "status")).Env())
	push := must(t, "push")(GitWorkspace(ctx, root, "push", "origin", "a:refs/heads/b"))
	if err := push.WithGitHTTPAuthHeader("Authorization: x"); err != nil {
		t.Fatal(err)
	}
	check("push with an auth header", push.Env())
	check("http-backend", must(t, "http-backend")(GitHTTPBackend(ctx, []string{"GIT_PROJECT_ROOT=" + root, "PATH_INFO=/x"})).Env())

	// Nothing composed: fail closed to an empty global config.
	restore2 := SetGitGlobalConfigForTest("")
	defer restore2()
	if v, _ := envValue(must(t, "unset")(GitWorkspace(ctx, root, "status")).Env(), "GIT_CONFIG_GLOBAL"); v != os.DevNull {
		t.Errorf("with nothing composed GIT_CONFIG_GLOBAL = %q, want %s", v, os.DevNull)
	}
}

// S6-D4 composition: the allowlisted keys survive, a filter definition and
// every other program key are dropped, includes are flattened, and each drop
// carries its reason.
func TestS6_D4_ComposeKeepsTheAllowlistAndDropsDrivers(t *testing.T) {
	entries := []GitConfigEntry{
		{Key: "user.name", Value: "Op"},
		{Key: "user.email", Value: "op@example.com"},
		{Key: "init.defaultbranch", Value: "main"},
		{Key: "safe.directory", Value: "*"},
		{Key: "credential.helper", Value: ""},
		{Key: "credential.https://github.com.helper", Value: "!/usr/bin/gh auth git-credential"},
		{Key: "http.proxy", Value: "http://proxy:3128"},
		{Key: "http.https://x.example.extraheader", Value: "X-A: b"},
		{Key: "url.git@github.com:.insteadof", Value: "https://github.com/"},
		{Key: "url.ssh://h/.pushinsteadof", Value: "https://h/"},
		{Key: "core.sshcommand", Value: "ssh -i /k"},
		{Key: "ssh.variant", Value: "ssh"},
		{Key: "core.excludesfile", Value: "~/.gitignore_global"},
		{Key: "core.autocrlf", Value: "input"},
		{Key: "core.eol", Value: "lf"},
		{Key: "core.quotepath", Value: "false"},
		{Key: "pull.rebase", Value: "true"},
		{Key: "push.default", Value: "simple"},
		{Key: "fetch.prune", Value: "true"},
		// dropped
		{Key: "filter.lfs.clean", Value: "git-lfs clean -- %f"},
		{Key: "filter.lfs.required", Value: "true"},
		{Key: "diff.external", Value: "/x"},
		{Key: "diff.foo.textconv", Value: "/x"},
		{Key: "merge.ours.driver", Value: "/x"},
		{Key: "core.hookspath", Value: "/h"},
		{Key: "core.attributesfile", Value: "/a"},
		{Key: "core.fsmonitor", Value: "/x"},
		{Key: "core.pager", Value: "less"},
		{Key: "core.editor", Value: "vi"},
		{Key: "include.path", Value: "/inc"},
		{Key: "includeif.gitdir:~/w/.path", Value: "/w"},
		{Key: "gpg.program", Value: "/x"},
		{Key: "commit.gpgsign", Value: "true"},
		{Key: "alias.st", Value: "!sh"},
		{Key: "core.gitproxy", Value: "/x"},
	}
	kept, dropped := FilterGitConfig(entries)
	keptKeys := map[string]bool{}
	for _, e := range kept {
		keptKeys[e.Key] = true
	}
	for _, k := range []string{"user.name", "user.email", "init.defaultbranch", "safe.directory",
		"credential.helper", "credential.https://github.com.helper", "http.proxy",
		"http.https://x.example.extraheader", "url.git@github.com:.insteadof", "url.ssh://h/.pushinsteadof",
		"core.sshcommand", "ssh.variant", "core.excludesfile", "core.autocrlf", "core.eol",
		"core.quotepath", "pull.rebase", "push.default", "fetch.prune"} {
		if !keptKeys[k] {
			t.Errorf("allowlisted key %s was dropped", k)
		}
	}
	droppedKeys := map[string]string{}
	for _, d := range dropped {
		if d.Reason == "" {
			t.Errorf("dropped key %s carries no reason", d.Key)
		}
		droppedKeys[d.Key] = d.Reason
	}
	for _, k := range []string{"filter.lfs.clean", "filter.lfs.required", "diff.external", "diff.foo.textconv",
		"merge.ours.driver", "core.hookspath", "core.attributesfile", "core.fsmonitor", "core.pager",
		"core.editor", "include.path", "includeif.gitdir:~/w/.path", "gpg.program", "commit.gpgsign",
		"alias.st", "core.gitproxy"} {
		if _, ok := droppedKeys[k]; !ok {
			t.Errorf("key %s must be dropped", k)
		}
		if keptKeys[k] {
			t.Errorf("key %s must not be kept", k)
		}
	}
	if len(kept)+len(dropped) != len(entries) {
		t.Errorf("examined %d entries, accounted for %d kept + %d dropped", len(entries), len(kept), len(dropped))
	}
}

// The composed file round-trips through real git: what git reads back is
// exactly the kept entries, in order (multi-valued keys included), with
// awkward values intact.
func TestS6_D4_RenderedConfigRoundTripsThroughGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	kept := []GitConfigEntry{
		{Key: "user.name", Value: `O'Brien "Op" \ tab	x`},
		{Key: "credential.helper", Value: ""},
		{Key: "credential.helper", Value: "!f() { echo \"a;b\"; }; f"},
		{Key: "url.https://x.example/a b.insteadof", Value: "gh:"},
		{Key: "http.sslverify", Value: "", NoValue: true},
	}
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, RenderGitConfig(kept), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "config", "--file", path, "--list", "-z").Output()
	if err != nil {
		t.Fatalf("git cannot read the rendered config: %v\n%s", err, RenderGitConfig(kept))
	}
	got := ParseGitConfigList(out)
	if len(got) != len(kept) {
		t.Fatalf("round trip: got %d entries %+v, want %d", len(got), got, len(kept))
	}
	for i := range kept {
		if got[i] != kept[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], kept[i])
		}
	}
}

// Composition end to end from an operator HOME: system config ignored when
// the operator's own GIT_CONFIG_NOSYSTEM says so, includes flattened, the file
// written under the data dir and registered for every git kind.
func TestS6_D4_ComposeFromTheOperatorsHome(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	inc := filepath.Join(home, "included")
	if err := os.WriteFile(inc, []byte("[user]\n\temail = inc@example.com\n[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(
		"[user]\n\tname = Op\n[include]\n\tpath = "+inc+"\n[credential]\n\thelper = !gh auth git-credential\n[core]\n\thooksPath = /h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	restore := SetGitGlobalConfigForTest("")
	defer restore()

	data := t.TempDir()
	res, err := ComposeGitConfig(context.Background(), data)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	want := filepath.Join(data, "git", "config")
	if res.Path != want || GitGlobalConfig() != want {
		t.Fatalf("composed path %q, registered %q, want %q", res.Path, GitGlobalConfig(), want)
	}
	out, err := exec.Command("git", "config", "--file", want, "--list").Output()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, k := range []string{"user.name=Op", "user.email=inc@example.com", "credential.helper=!gh auth git-credential"} {
		if !strings.Contains(s, k) {
			t.Errorf("composed config lacks %q:\n%s", k, s)
		}
	}
	for _, k := range []string{"filter.lfs", "core.hookspath", "include.path"} {
		if strings.Contains(s, k) {
			t.Errorf("composed config kept %q:\n%s", k, s)
		}
	}
	var dropped []string
	for _, d := range res.Dropped {
		dropped = append(dropped, d.Key)
	}
	for _, k := range []string{"filter.lfs.clean", "core.hookspath", "include.path"} {
		if !strings.Contains(strings.Join(dropped, " "), k) {
			t.Errorf("the report does not list dropped key %s (dropped: %v)", k, dropped)
		}
	}
	if res.Examined != len(res.Kept)+len(res.Dropped) || res.Examined == 0 {
		t.Errorf("denominator: examined %d, kept %d, dropped %d", res.Examined, len(res.Kept), len(res.Dropped))
	}
	if st, err := os.Stat(want); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("composed config mode: %v %v", st, err)
	}
}
