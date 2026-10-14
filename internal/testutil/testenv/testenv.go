// Package testenv gives tests the environment CI gives them, on any host.
//
// A developer shell can export a production daemon's VORNIK_* settings (its
// data directory, its config tree, its API keys); CI exports none. A test that
// passes that environment on to a daemon it starts, or reads it in-process,
// works against production state on that host and against nothing in CI, so
// CI cannot catch it. GitHub #357: an e2e daemon inherited VORNIK_DATA_DIR,
// fell back to production's workspace root and pruned live task worktrees;
// ordinary unit tests rewrote production's composed git config
// (testing design 19, sections 10 and 11).
//
// Only _test.go files import this package (architecture law L4).
package testenv

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// inherited is what Clean passes through from the test process: what building
// and running a program needs, and nothing that names a daemon's state or
// carries a secret.
var inherited = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR", "XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS", // rootless podman's systemd cgroup manager
	"GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOTOOLCHAIN",
	"GOCOVERDIR", // a -cover daemon writes the e2e coverage profile here
	"CONTAINER_HOST",
}

// Clean is the ONLY environment a test gives a process it starts. The
// allowlist is read from the test's own environment at call time (so a
// t.Setenv("HOME", …) is honoured); extra entries follow it, and for a
// duplicated key the last entry wins (os/exec keeps the last value).
// No VORNIK_* variable is ever inherited: a test sets each one it needs.
func Clean(extra ...string) []string {
	env := []string{"NO_PROXY=*", "no_proxy=*"}
	for _, k := range inherited {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, extra...)
}

// Daemon is Clean for a daemon process: it also gives the daemon its own
// VORNIK_DATA_DIR, so a config that names no runtime.project_workspace_path
// cannot resolve a workspace root another daemon owns (#357). It panics on an
// empty dataDir, and when extra names no VORNIK_CONFIG: without one the
// daemon's config resolution falls back to $XDG_CONFIG_HOME/vornik and
// $HOME/.config/vornik, which on a developer host is production's config
// (testing design 19 §10.1, §11.6).
func Daemon(dataDir string, extra ...string) []string {
	if strings.TrimSpace(dataDir) == "" {
		panic("testenv.Daemon: a test daemon needs its own data directory")
	}
	hasConfig := false
	for _, kv := range extra {
		if v, ok := strings.CutPrefix(kv, "VORNIK_CONFIG="); ok && strings.TrimSpace(v) != "" {
			hasConfig = true
		}
	}
	if !hasConfig {
		panic("testenv.Daemon: a test daemon needs an explicit VORNIK_CONFIG (else it reads the user's ~/.config/vornik)")
	}
	return Clean(append([]string{"VORNIK_DATA_DIR=" + dataDir}, extra...)...)
}

// isTestKnob reports whether a VORNIK_* variable is one a test reads itself
// to decide how to run (never one the daemon reads), so Main keeps it.
func isTestKnob(key string) bool {
	for _, p := range []string{"VORNIK_E2E_", "VORNIK_TEST_", "VORNIK_REPLAY_"} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return key == "VORNIK_INTEGRATION"
}

// Isolate is Main's setup step, exposed for its own tests: it unsets every
// inherited VORNIK_* except the test knobs and points VORNIK_DATA_DIR at a
// fresh directory. It returns that directory and the names it unset.
func Isolate() (dataDir string, unset []string, err error) {
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "VORNIK_") || isTestKnob(key) {
			continue
		}
		if err := os.Unsetenv(key); err != nil {
			return "", unset, err
		}
		unset = append(unset, key)
	}
	dataDir, err = os.MkdirTemp("", "vornik-test-data-")
	if err != nil {
		return "", unset, err
	}
	if err := os.Setenv("VORNIK_DATA_DIR", dataDir); err != nil {
		_ = os.RemoveAll(dataDir)
		return "", unset, err
	}
	return dataDir, unset, nil
}

// Main is the TestMain of a package whose tests build a daemon in-process
// (a service.Container), or start processes (test/e2e, test/integration): it
// isolates the environment, runs the tests and removes the data directory.
// Use it as
//
//	func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }
//
// A package with its own TestMain body passes it as run; it then runs inside
// the isolated environment:
//
//	func TestMain(m *testing.M) { os.Exit(testenv.Main(m, mainE2E)) }
func Main(m *testing.M, run ...func(*testing.M) int) int {
	dataDir, _, err := Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testenv: isolate the test environment: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dataDir) }()
	if len(run) > 0 && run[0] != nil {
		return run[0](m)
	}
	return m.Run()
}
