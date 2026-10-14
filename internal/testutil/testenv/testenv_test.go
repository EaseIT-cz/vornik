package testenv

import (
	"os"
	"strings"
	"testing"
)

// The processes a test starts must never inherit production settings: a
// Hermes-lane daemon that inherited VORNIK_CONFIGS_DIR loaded the real
// projects (bring-up incident 2026-09-30), and an e2e daemon that inherited
// VORNIK_DATA_DIR pruned production worktrees (GitHub #357). Moved here from
// the Hermes lane when its cleanEnv became this package's Clean.
func TestCleanDropsInheritedVornikAndSecrets(t *testing.T) {
	t.Setenv("VORNIK_CONFIGS_DIR", "/prod/configs")
	t.Setenv("VORNIK_DATA_DIR", "/prod/data")
	t.Setenv("VORNIK_API_KEY", "prod-key")
	t.Setenv("SOME_API_TOKEN", "secret")
	env := Clean("VORNIK_CONFIG=/tmp/lane/config.yaml")
	for _, kv := range env {
		if strings.HasPrefix(kv, "VORNIK_") && kv != "VORNIK_CONFIG=/tmp/lane/config.yaml" {
			t.Errorf("inherited %s", kv)
		}
		if strings.Contains(kv, "prod-key") || strings.Contains(kv, "secret") || strings.Contains(kv, "/prod/") {
			t.Errorf("leaked %s", kv)
		}
	}
}

func TestCleanReadsTheAllowlistAtCallTime(t *testing.T) {
	t.Setenv("HOME", "/test/home")
	t.Setenv("GOCOVERDIR", "/test/cov")
	env := Clean("PATH=/shim")
	if !contains(env, "HOME=/test/home") || !contains(env, "GOCOVERDIR=/test/cov") {
		t.Errorf("allowlisted values missing: %v", env)
	}
	if env[len(env)-1] != "PATH=/shim" {
		t.Errorf("extra entries must come last (the last value of a key wins): %v", env)
	}
}

// audit 2026-10-13 (#357): every daemon a test starts gets its own data dir.
func TestDaemonGivesTheDaemonItsOwnDataDir(t *testing.T) {
	t.Setenv("VORNIK_DATA_DIR", "/prod/data")
	env := Daemon("/tmp/own", "VORNIK_CONFIG=/tmp/c.yaml")
	if !contains(env, "VORNIK_DATA_DIR=/tmp/own") || !contains(env, "VORNIK_CONFIG=/tmp/c.yaml") {
		t.Fatalf("daemon env = %v", env)
	}
	if contains(env, "VORNIK_DATA_DIR=/prod/data") {
		t.Fatalf("inherited the shell's data dir: %v", env)
	}
	mustPanic(t, "an empty data dir", func() { Daemon(" ", "VORNIK_CONFIG=/tmp/c.yaml") })
	// Without VORNIK_CONFIG the daemon falls back to the user's
	// ~/.config/vornik: on a developer host, production's config.
	mustPanic(t, "no VORNIK_CONFIG", func() { Daemon("/tmp/own") })
	mustPanic(t, "an empty VORNIK_CONFIG", func() { Daemon("/tmp/own", "VORNIK_CONFIG=") })
}

func mustPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s must panic", what)
		}
	}()
	f()
}

// audit 2026-10-13 (#357): an in-process daemon built by a unit test wrote
// $VORNIK_DATA_DIR/git/config into the shell's (production's) data dir.
func TestIsolateRedirectsTheDataDirAndKeepsOnlyTestKnobs(t *testing.T) {
	t.Setenv("VORNIK_DATA_DIR", "/prod/data")
	t.Setenv("VORNIK_CONFIGS_DIR", "/prod/configs")
	t.Setenv("VORNIK_API_KEY", "prod-key")
	t.Setenv("VORNIK_E2E_CACHE", "/cache")
	t.Setenv("VORNIK_TEST_ALLOW_DAEMON", "1")
	t.Setenv("VORNIK_INTEGRATION", "1")
	t.Setenv("VORNIK_REPLAY_RECORD", "1")

	dir, unset, err := Isolate()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if got := os.Getenv("VORNIK_DATA_DIR"); got != dir || got == "/prod/data" {
		t.Fatalf("VORNIK_DATA_DIR = %q, want the fresh %q", got, dir)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("data dir %q not created: %v", dir, err)
	}
	for _, k := range []string{"VORNIK_CONFIGS_DIR", "VORNIK_API_KEY"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s still set", k)
		}
		if !contains(unset, k) {
			t.Errorf("%s not reported as unset: %v", k, unset)
		}
	}
	for _, k := range []string{"VORNIK_E2E_CACHE", "VORNIK_TEST_ALLOW_DAEMON", "VORNIK_INTEGRATION", "VORNIK_REPLAY_RECORD"} {
		if _, ok := os.LookupEnv(k); !ok {
			t.Errorf("test knob %s was removed", k)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
