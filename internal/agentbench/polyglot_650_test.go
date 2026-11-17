package agentbench

// GitHub #650 phase 1 (benchmark design, Amendment 2026-11-16): tasks borrowed
// from Aider Polyglot need a seed (the exercise's stubs, put into the cleared
// target before each repeat), Python suites in the suite digest, and a scaffold
// axis so a bare-model journal never compares with a swarm one.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSeedDir_ResolvesAgainstTheTaskSetAndStaysInside_650(t *testing.T) {
	base := t.TempDir()
	task := TaskSpec{ID: "pg-go-leap", Seed: "polyglot/go/leap/seed"}.WithAttachmentBase(base)
	got, err := task.SeedDir()
	if err != nil || got != filepath.Join(base, "polyglot/go/leap/seed") {
		t.Fatalf("SeedDir = %q, %v", got, err)
	}
	if got, err := (TaskSpec{ID: "x"}).SeedDir(); err != nil || got != "" {
		t.Fatalf("no seed: %q, %v", got, err)
	}
	for _, bad := range []string{"../outside", "/abs/path", ".."} {
		if _, err := (TaskSpec{ID: "x", Seed: bad}.WithAttachmentBase(base)).SeedDir(); err == nil {
			t.Errorf("seed %q escaped the task-set directory", bad)
		}
	}
}

func TestValidateTaskTargets_ASeededTaskHasExactlyOneTarget_650(t *testing.T) {
	ok := []TaskSpec{{ID: "a", Prompt: "solve polyglot/go/leap", Seed: "s", Targets: []string{"polyglot/go/leap"}}}
	if err := ValidateTaskTargets(ok, true); err != nil {
		t.Fatalf("one target refused: %v", err)
	}
	two := []TaskSpec{{ID: "a", Prompt: "polyglot/go/a polyglot/go/b", Seed: "s", Targets: []string{"polyglot/go/a", "polyglot/go/b"}}}
	if err := ValidateTaskTargets(two, true); err == nil || !strings.Contains(err.Error(), "seed") {
		t.Fatalf("a seeded task with two targets was accepted: %v", err)
	}
}

func TestTaskBody_UnchangedWithoutASeedAndMovedByOne_650(t *testing.T) {
	base := t.TempDir()
	plain := TaskSpec{ID: "t", Workflow: "dev-pipeline", Prompt: "p"}
	body, err := TaskBody(plain)
	if err != nil || body != "dev-pipeline\x00p" {
		t.Fatalf("a task without a seed must keep its digest body: %q, %v", body, err)
	}
	writeFile(t, filepath.Join(base, "seed/leap.go"), "package leap\n")
	seeded := TaskSpec{ID: "t", Workflow: "dev-pipeline", Prompt: "p", Seed: "seed"}.WithAttachmentBase(base)
	b1, err := TaskBody(seeded)
	if err != nil || b1 == body {
		t.Fatalf("the seed must be in the body: %q, %v", b1, err)
	}
	writeFile(t, filepath.Join(base, "seed/leap.go"), "package leap // changed\n")
	b2, _ := TaskBody(seeded)
	if b2 == b1 {
		t.Fatal("a changed stub must change the task's digest body")
	}
}

func TestAcceptanceSetDigest_PythonSuitesCount_650(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "suite/leap_test.py"), "def test_leap(): pass\n")
	task := TaskSpec{ID: "pg-py-leap", Acceptance: "suite"}.WithAttachmentBase(base)
	d1, err := AcceptanceSetDigest([]TaskSpec{task})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(base, "suite/leap_test.py"), "def test_leap(): assert False\n")
	d2, _ := AcceptanceSetDigest([]TaskSpec{task})
	if d1 == d2 {
		t.Fatal("a changed Python suite must change the acceptance digest")
	}
	// A Go-only suite keeps the digest it had before Python was hashed.
	writeFile(t, filepath.Join(base, "gosuite/zz_acceptance_x_test.go"), "package x\n")
	goTask := TaskSpec{ID: "g", Acceptance: "gosuite"}.WithAttachmentBase(base)
	withPy := TaskSpec{ID: "g", Acceptance: "gosuite"}.WithAttachmentBase(base)
	dGo, _ := AcceptanceSetDigest([]TaskSpec{goTask})
	writeFile(t, filepath.Join(base, "gosuite/extra_test.py"), "def test_x(): pass\n")
	dGoPy, _ := AcceptanceSetDigest([]TaskSpec{withPy})
	if dGo == dGoPy {
		t.Fatal("a Go-and-Python suite must not share a digest with the Go-only one")
	}
}

func TestScaffoldAxis_RefusesBareAgainstSwarmAndDefaultsOldJournals_650(t *testing.T) {
	swarm := ArmFields{HarnessVersion: HarnessVersion, Name: "a", Scaffold: ""}
	explicit := swarm
	explicit.Scaffold = ScaffoldSwarm
	if err := CheckComparable(swarm, explicit); err != nil {
		t.Fatalf("a journal without a scaffold must read as swarm: %v", err)
	}
	bare := swarm
	bare.Scaffold = ScaffoldBarePass1
	if err := CheckComparable(swarm, bare); err == nil || !strings.Contains(err.Error(), "scaffold") {
		t.Fatalf("a bare journal compared with a swarm one: %v", err)
	}
	found := false
	for _, a := range ComparabilityAxes() {
		found = found || a == "scaffold"
	}
	if !found {
		t.Fatal("scaffold is not a comparability axis")
	}
}
