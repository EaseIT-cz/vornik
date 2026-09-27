package architecture

import (
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The process-spawn law (https://docs.vornik.io,
// operator ruling 2026-08-03: only vornikctl may spawn processes). A daemon
// executes only the orchestration it owns, so os/exec may appear in daemon
// code only where the design has placed it. This law pins that on the IMPORT
// graph: every package linked into a daemon main that imports os/exec must be
// named below, with the slice that removes it; a new one fails the build. The
// list only shrinks: an entry whose package no longer imports os/exec fails
// too, so it cannot outlive its violation.
//
// Self-contained and module-agnostic like deps_install_law_test.go: it ships in
// the Community export, where some packages below do not exist, so an entry
// whose package is absent from the graph is skipped rather than failed.

// spawnLawDaemonMains are the mains that run as a daemon or a network-facing
// service. Other mains are excluded for a stated reason:
//   - cmd/vornikctl: the one process the ruling allows to spawn.
//   - cmd/docs-gen, cmd/skills-site, cmd/lint-lld-contracts, cmd/reachability:
//     build and development tools, never deployed.
//   - cmd/agent-helper, cmd/mcp-bridge: run INSIDE the agent sandbox.
//   - cmd/vornik-images: an operator image tool run from a shell.
//   - cmd/ui-smoke: a test harness.
var spawnLawDaemonMains = []string{"cmd/vornik-enterprise", "cmd/vornik", "cmd/vornik-broker"}

// spawnPackage is the one daemon-linked package the law permits to import
// os/exec (design §3): every other spawn goes through its closed set of kinds.
// It is not an exception and never leaves; TestSpawnLaw_OnlyNamedPackagesImportSpawn
// pins who may reach it.
const spawnPackage = "internal/spawn"

// spawnLawExceptions: module packages linked into a daemon that import os/exec
// other than internal/spawn, each with the reason it is not yet confined. Paths
// are relative to the module. S1b-2 moved the orchestration packages (runtime,
// executor, executor/handlers/forge, forge/github, autonomy), the
// configured-program packages (mcp, chat), the sandbox runner (sandboxtool),
// git HTTP (api) and the forge rev-parse (service) onto internal/spawn.
var spawnLawExceptions = map[string]string{
	"internal/agentloop": "git tools called only by the in-sandbox agent-helper; the daemon's config assistant refuses git_* (design §3, round-1 F8)",
	"internal/report":    "vornikctl-only JournalTail; linked, no daemon caller (design §2.4)",
	// The daemon main itself.
	"cmd/vornik-enterprise": "the operator-typed `migrate-ce` subcommand runs systemctl; never reached by the running daemon",
}

// spawnLawThirdParty: dependencies linked into a daemon that import os/exec.
var spawnLawThirdParty = map[string]string{
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds": "AWS credential_process: a program named in the operator's AWS shared config (reading 3)",
	"modernc.org/libc": "imports os/exec; no path from the daemon uses it",
}

func TestSpawnLaw_DaemonCodeImportsOSExecOnlyWhereTheDesignPlacesIt(t *testing.T) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	root := filepath.Dir(strings.TrimSpace(string(out)))
	module := depsGoList(t, root, "-m")[0]

	importers := map[string]bool{} // packages in any daemon's graph that import os/exec
	inGraph := map[string]bool{}
	for _, main := range spawnLawDaemonMains {
		if !depsGoListOK(root, "./"+main) {
			continue // absent in this edition
		}
		for _, line := range depsGoList(t, root, "-deps", "-f", `{{.ImportPath}} {{join .Imports " "}}`, "./"+main) {
			fields := strings.Fields(line)
			pkg := fields[0]
			inGraph[pkg] = true
			for _, imp := range fields[1:] {
				if imp == "os/exec" {
					importers[pkg] = true
				}
			}
		}
	}

	var unexpected []string
	for pkg := range importers {
		if rel, ok := strings.CutPrefix(pkg, module+"/"); ok {
			if rel == spawnPackage {
				continue
			}
			if _, allowed := spawnLawExceptions[rel]; !allowed {
				unexpected = append(unexpected, rel)
			}
			continue
		}
		if !strings.Contains(strings.SplitN(pkg, "/", 2)[0], ".") {
			continue // the standard library
		}
		if _, allowed := spawnLawThirdParty[pkg]; !allowed {
			unexpected = append(unexpected, pkg)
		}
	}
	sort.Strings(unexpected)
	for _, p := range unexpected {
		t.Errorf("%s is linked into a daemon and imports os/exec, which the process-spawn law does not place there. "+
			"Spawn through internal/spawn's kinds, move the work to vornikctl or the sandbox, or add it to "+
			"spawnLawExceptions with the design slice that removes it.", p)
	}

	// The list only shrinks: an exception whose package is in the graph but no
	// longer imports os/exec must be removed.
	for rel := range spawnLawExceptions {
		pkg := module + "/" + rel
		if inGraph[pkg] && !importers[pkg] {
			t.Errorf("%s no longer imports os/exec: remove it from spawnLawExceptions", rel)
		}
	}
	for pkg := range spawnLawThirdParty {
		if inGraph[pkg] && !importers[pkg] {
			t.Errorf("%s no longer imports os/exec: remove it from spawnLawThirdParty", pkg)
		}
	}
}

// The host doctor (process-spawn law, S2) runs podman, skopeo, systemctl and
// git by design, so it may be linked only by vornikctl: never by a daemon main,
// where a request could reach it. vornikctl must link it, so the law cannot
// pass by the package quietly going unused.
func TestSpawnLaw_HostDoctorIsVornikctlOnly(t *testing.T) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	root := filepath.Dir(strings.TrimSpace(string(out)))
	module := depsGoList(t, root, "-m")[0]
	hostOnly := []string{module + "/internal/hostdoctor", module + "/internal/imagemanifest/hostprobe"}

	links := func(main, pkg string) bool {
		for _, dep := range depsGoList(t, root, "-deps", "./"+main) {
			if dep == pkg {
				return true
			}
		}
		return false
	}
	for _, main := range spawnLawDaemonMains {
		if !depsGoListOK(root, "./"+main) {
			continue
		}
		for _, pkg := range hostOnly {
			if links(main, pkg) {
				t.Errorf("%s links %s, which runs host programs: only vornikctl may", main, pkg)
			}
		}
	}
	if depsGoListOK(root, "./cmd/vornikctl") && !links("cmd/vornikctl", module+"/internal/hostdoctor") {
		t.Error("cmd/vornikctl no longer links internal/hostdoctor: the host checks would run nowhere")
	}
}

// depsGoListOK reports whether go list resolves a package, so a main absent from
// this edition is skipped rather than failed.
func depsGoListOK(dir, pkg string) bool {
	cmd := exec.Command("go", "list", pkg)
	cmd.Dir = dir
	return cmd.Run() == nil
}
