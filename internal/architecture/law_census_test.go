package architecture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The law census (#334 item 6, "lints that cannot pass by not looking"; CI
// pipeline design amendment 2026-10-13). A law that walks the tree or a
// package graph reports no violation when it walked nothing, so "clean" and
// "never looked" read the same. requireExamined logs the law's denominator and
// fails it below a floor. Lives in its own file, like moduleRoot, so every law
// that ships to the Community export has it.

// requireExamined logs "<law>: examined N <what>" and fails when N < floor.
func requireExamined(t testing.TB, law, what string, n, floor int) bool {
	t.Helper()
	t.Logf("%s: examined %d %s", law, n, what)
	if n < floor {
		t.Errorf("%s: examined %d %s (floor %d); a law that looked at nothing has not passed", law, n, what, floor)
		return false
	}
	return true
}

// enterpriseTree reports whether root is the Enterprise tree (its daemon main
// exists). A law that may skip "absent from this edition" in the Community
// export must not skip here.
func enterpriseTree(root string) bool {
	_, err := os.Stat(filepath.Join(root, "cmd", "vornik-enterprise", "main.go"))
	return err == nil
}

// absentFromEdition is the one way a law may skip because the package it
// guards is missing: in the Community export (no Enterprise daemon main) it
// skips with the reason; in the Enterprise tree, where every guarded package
// exists, the absence means the law examined nothing, which fails.
func absentFromEdition(t testing.TB, root, what string) {
	t.Helper()
	if enterpriseTree(root) {
		t.Fatalf("%s is absent from the Enterprise tree: the law examined nothing and has not passed (#334 item 6)", what)
	}
	t.Skipf("%s is absent from this edition", what)
}

// recordingTB captures Errorf/Fatalf so a test can assert that a law's floor
// fires, without failing the test that drives it.
type recordingTB struct {
	testing.TB
	errs []string
}

type recordedFatal struct{}

func (r *recordingTB) Helper()             {}
func (r *recordingTB) Log(...any)          {}
func (r *recordingTB) Logf(string, ...any) {}
func (r *recordingTB) Error(args ...any)   { r.errs = append(r.errs, fmt.Sprint(args...)) }
func (r *recordingTB) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}
func (r *recordingTB) Fatal(args ...any) { r.Error(args...); panic(recordedFatal{}) }
func (r *recordingTB) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	panic(recordedFatal{})
}
func (r *recordingTB) Skip(args ...any) {
	r.Error(append([]any{"SKIP: "}, args...)...)
	panic(recordedFatal{})
}
func (r *recordingTB) Skipf(format string, args ...any) {
	r.Errorf("SKIP: "+format, args...)
	panic(recordedFatal{})
}

// recordLaw runs a law against a recordingTB and returns what it reported.
func recordLaw(t *testing.T, law func(testing.TB)) []string {
	t.Helper()
	r := &recordingTB{TB: t}
	func() {
		defer func() {
			if p := recover(); p != nil {
				if _, ok := p.(recordedFatal); !ok {
					panic(p)
				}
			}
		}()
		law(r)
	}()
	return r.errs
}

func TestRequireExamined_FailsBelowTheFloor(t *testing.T) {
	r := &recordingTB{TB: t}
	if requireExamined(r, "law", "files", 0, 1) || len(r.errs) != 1 {
		t.Fatalf("examined 0 below a floor of 1 must fail; errs=%q", r.errs)
	}
	r = &recordingTB{TB: t}
	if !requireExamined(r, "law", "files", 120, 100) || len(r.errs) != 0 {
		t.Fatalf("examined 120 over a floor of 100 must pass; errs=%q", r.errs)
	}
}

// audit 2026-10-12 (#334 item 6): the spawn law skipped every daemon main that
// `go list` could not resolve and printed nothing, so renaming the mains (or a
// broken module) left a law that examined no graph and passed. The host-doctor
// law did the same, and its vornikctl check was gated on vornikctl resolving.
func TestSpawnLaw_RefusesToPassOverNothing(t *testing.T) {
	root, module := moduleRootAndPath(t)
	nothing := []string{"cmd/no-such-daemon"}
	if errs := recordLaw(t, func(tb testing.TB) { spawnLawCheck(tb, root, module, nothing) }); len(errs) == 0 {
		t.Error("the spawn law over mains that resolve to nothing must fail, it passed")
	}
	if errs := recordLaw(t, func(tb testing.TB) { hostDoctorCheck(tb, root, module, nothing, "cmd/no-such-ctl") }); len(errs) == 0 {
		t.Error("the host-doctor law over mains and a vornikctl that resolve to nothing must fail, it passed")
	}
}

// audit 2026-10-12 (#334 item 6): the agentloop-git and spawn-importer laws
// t.Skip'ped when their package was missing from `go list ./...`, which in the
// Enterprise tree means the law examined nothing, not that the edition lacks it.
func TestAbsentFromEdition_FailsInEnterpriseSkipsInCommunity(t *testing.T) {
	root, _ := moduleRootAndPath(t)
	if !enterpriseTree(root) {
		t.Skip("Community tree: the Enterprise arm cannot be driven here")
	}
	errs := recordLaw(t, func(tb testing.TB) { absentFromEdition(tb, root, "internal/x") })
	if len(errs) != 1 || strings.HasPrefix(errs[0], "SKIP") {
		t.Fatalf("in the Enterprise tree an absent package must FAIL, got %q", errs)
	}
	errs = recordLaw(t, func(tb testing.TB) { absentFromEdition(tb, t.TempDir(), "internal/x") })
	if len(errs) != 1 || !strings.HasPrefix(errs[0], "SKIP") {
		t.Fatalf("in a Community tree an absent package skips with its reason, got %q", errs)
	}
}

func TestEnterpriseTree_RecognisesThisTree(t *testing.T) {
	root, _ := moduleRootAndPath(t)
	if _, err := os.Stat(filepath.Join(root, "cmd", "vornik-enterprise")); err == nil && !enterpriseTree(root) {
		t.Fatal("cmd/vornik-enterprise exists but enterpriseTree says Community")
	}
	if enterpriseTree(t.TempDir()) {
		t.Fatal("an empty directory is not the Enterprise tree")
	}
}
