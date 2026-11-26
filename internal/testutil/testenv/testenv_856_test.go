package testenv

// GitHub #856 (testing design 19 §11.1): a git a test starts runs its
// auto-maintenance in the foreground, so no detached git writes into a
// t.TempDir while the test's cleanup removes it.
//
// Found at: unit (CE export gate, TempDir cleanup of
// TestMirror_CapCountsIngestsOnly_639: "unlinkat .../.git: directory not
// empty"), observed at unit.

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val // the last one wins, as in the process
		}
	}
	return v
}

func TestClean856_GitMaintenanceRunsInTheForeground(t *testing.T) {
	t.Setenv("GIT_CONFIG_PARAMETERS", "'maintenance.autoDetach'='true'") // never inherited
	if got := envOf(Clean(), "GIT_CONFIG_PARAMETERS"); got != GitMaintenanceForeground {
		t.Fatalf("GIT_CONFIG_PARAMETERS = %q, want %q", got, GitMaintenanceForeground)
	}
	if got := envOf(Clean("GIT_CONFIG_PARAMETERS=x"), "GIT_CONFIG_PARAMETERS"); got != "x" {
		t.Fatalf("a test's own value in extra = %q, want x", got)
	}

	// A real fetch with Clean's environment, read with trace2.
	dir := t.TempDir()
	up, clone, trace := filepath.Join(dir, "up"), filepath.Join(dir, "clone"), filepath.Join(dir, "trace2.json")
	env := Clean("GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	git := func(extra []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(append([]string(nil), env...), extra...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	commit := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(up, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(nil, "-C", up, "add", "-A")
		git(nil, "-C", up, "commit", "-q", "-m", name)
	}
	git(nil, "init", "-q", "-b", "main", up)
	commit("a")
	git(nil, "clone", "-q", up, clone)
	commit("b")
	git([]string{"GIT_TRACE2_EVENT=" + trace}, "-C", clone, "fetch", "-q", "origin")

	f, err := os.Open(trace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	runs := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Event != "child_start" || len(ev.Argv) < 2 || ev.Argv[1] != "maintenance" {
			continue
		}
		runs++
		if !strings.Contains(strings.Join(ev.Argv, " "), "--no-detach") {
			t.Errorf("the fetch started maintenance as %q, want --no-detach", ev.Argv)
		}
	}
	if runs == 0 {
		t.Fatal("the fetch started no maintenance run; this git no longer runs it after a fetch, so the test examined nothing")
	}
}
