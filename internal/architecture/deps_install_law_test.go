package architecture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The daemon never installs project dependencies (project dependency
// provisioning design §8.2, 2026-09-25). Only `vornikctl deps install` may: a
// daemon install would be triggered by a project config editable through the
// control plane, which the 2026-08-03 ruling forbids ("only vornikctl may
// spawn processes"), and it would use the host's Python rather than the agent
// image's.
//
// The law is on the IMPORT graph, transitively, not on names: nothing that can
// fetch lives outside internal/projectdeps/install, and no main but vornikctl
// may link that package. vornikctl must link it, so the law cannot pass by the
// package quietly going unused.
//
// Self-contained and module-agnostic: it ships in the Community export, whose
// module path differs and whose import_law_test.go is replaced by a template.

// depsInstallLinkAllowed: mains that link the installer only because they link
// the vornikctl command tree (internal/cli) to RENDER it, and never execute a
// command. Each needs its reason; a daemon never belongs here. Paths are
// relative to the module.
var depsInstallLinkAllowed = map[string]string{
	"cmd/docs-gen":    "renders the CLI reference from the cobra tree at build time; runs no command",
	"cmd/skills-site": "renders the CLI tree into the skills site at build time; runs no command",
}

func depsGoList(t *testing.T, dir string, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list %v: %v\n%s", args, err, stderr)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestDaemonMainsDoNotLinkTheDependencyInstaller(t *testing.T) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	root := filepath.Dir(strings.TrimSpace(string(out)))
	module := depsGoList(t, root, "-m")[0]
	installer := module + "/internal/projectdeps/install"
	vornikctl := module + "/cmd/vornikctl"

	links := func(main string) bool {
		for _, dep := range depsGoList(t, root, "-deps", main) {
			if dep == installer {
				return true
			}
		}
		return false
	}
	// EVERY main under cmd/ except vornikctl, so a new daemon, sidecar or tool
	// main cannot link the installer unnoticed (review-20260925-0325 F5).
	for _, main := range depsGoList(t, root, "./cmd/...") {
		if main == vornikctl {
			continue
		}
		if _, ok := depsInstallLinkAllowed[strings.TrimPrefix(main, module+"/")]; ok {
			continue
		}
		if links(main) {
			t.Errorf("%s links %s: only vornikctl may install dependencies (design §8.2)", main, installer)
		}
	}
	if !links(vornikctl) {
		t.Errorf("vornikctl does not link %s, so the law above is vacuous: `vornikctl deps install` is the one permitted caller", installer)
	}
}
