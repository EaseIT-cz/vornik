package configdrift

import (
	"strings"
	"testing"

	"github.com/pmezard/go-difflib/difflib"
)

// Slice E (drift design, the pre-apply snapshot): a `d` hunk an APPROVED
// remove_step / reorder_steps explains is exempt — positionally: its lines must
// align, in an order-preserving match of the template against the pre-apply
// file, contiguously into a range the approved change deleted.

// exemptPre is the pre-apply file every case starts from: steps a then b.
const exemptPre = "steps:\n" + stepA + stepB

func exemptFor(t *testing.T, tmpl, deployed string) map[int]bool {
	t.Helper()
	f := newFixture(t)
	f.write(".templates/workflows/w.md", tmpl)
	f.write("workflows/w.md", deployed)
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil {
		t.Fatal("no finding")
	}
	return ff.ExemptHunks([]ApprovedChange{{PreApply: []byte(exemptPre), PostApply: []byte(deployed)}})
}

const (
	stepA = "  a:\n    role: coder\n    retry:\n      max: 3\n      backoff: 30s\n"
	stepB = "  b:\n    role: reviewer\n"
)

// The removed step, still in the template, is exempt: the operator approved
// its removal.
func TestExempt_TheRemovedStepIsExempt(t *testing.T) {
	tmpl := "steps:\n" + stepA + stepB
	deployed := "steps:\n" + stepB
	got := exemptFor(t, tmpl, deployed)
	if !got[0] {
		t.Fatalf("the approved removal was not exempt: %v", got)
	}
}

// A NEW template step sharing the removed step's retry block, placed AFTER b
// (the order-preserving match cannot bind it back before b), is reported.
func TestExempt_BoilerplateCollisionElsewhereIsReported(t *testing.T) {
	stepC := "  c:\n    role: tester\n    retry:\n      max: 3\n      backoff: 30s\n"
	tmpl := "steps:\n" + stepB + stepC
	deployed := "steps:\n" + stepB
	if got := exemptFor(t, tmpl, deployed); len(got) != 0 {
		t.Fatalf("a never-taken step sharing the removed step's boilerplate was exempt: %v", got)
	}
}

// The adversarial shape: the new step sits exactly WHERE the removed step was,
// so its identical retry body can align into the removed span — but its own
// heading has no counterpart in the pre-apply file, so the hunk is reported.
func TestExempt_BoilerplateCollisionInPlaceIsReported(t *testing.T) {
	stepC := "  c:\n    role: tester\n    retry:\n      max: 3\n      backoff: 30s\n"
	tmpl := "steps:\n" + stepC + stepB
	deployed := "steps:\n" + stepB
	pre := "steps:\n" + stepA + stepB
	if got := exemptFor(t, tmpl, deployed); len(got) != 0 {
		t.Fatalf("a never-taken step placed where the removed one was was exempt: %v", got)
	}
	// Pin the alignment path (round 16): in this same context the shared
	// retry body DOES align into the removed span — only the heading lines
	// keep the hunk reported.
	f := newFixture(t)
	f.write(".templates/workflows/w.md", tmpl)
	f.write("workflows/w.md", deployed)
	ff := findFile(f.compare(), "workflows/w.md")
	body := 0
	for _, op := range difflib.NewMatcherWithJunk(ff.tmplLines, splitLines([]byte(pre)), false, nil).GetOpCodes() {
		if op.Tag == 'e' && op.J1 < 6 && op.J2 > 3 {
			body++
		}
	}
	if body == 0 {
		t.Fatal("the shared body did not align into the removed span; the test no longer exercises the collision")
	}
}

// A template edit to the removed step, made after the operator removed it, is
// a replace against the pre-apply file — not aligned, so reported.
func TestExempt_ATemplateEditToTheRemovedStepIsReported(t *testing.T) {
	pre := "steps:\n" + stepA + stepB
	tmpl := strings.Replace(pre, "backoff: 30s", "backoff: 60s", 1)
	deployed := "steps:\n" + stepB
	if got := exemptFor(t, tmpl, deployed); len(got) != 0 {
		t.Fatalf("a template edit to the removed step was exempt: %v", got)
	}
}

// A template fix the deployment never took, unrelated to the removal.
func TestExempt_ANeverTakenFixIsReported(t *testing.T) {
	tmpl := "steps:\n" + stepB + "    recovery: true\n"
	deployed := "steps:\n" + stepB
	if got := exemptFor(t, tmpl, deployed); len(got) != 0 {
		t.Fatalf("an unrelated never-taken fix was exempt: %v", got)
	}
}

// A reorder: the moved block's delete half is exempt.
func TestExempt_AReorderedBlockIsExempt(t *testing.T) {
	tmpl := "steps:\n" + stepA + stepB
	deployed := "steps:\n" + stepB + stepA
	got := exemptFor(t, tmpl, deployed)
	if len(got) == 0 {
		t.Fatalf("an approved reorder's moved block was not exempt")
	}
}

// Union: a hunk explained by the SECOND of two approved changes is exempt.
func TestExempt_UnionAcrossChanges(t *testing.T) {
	tmpl := "steps:\n" + stepA + stepB
	deployed := "steps:\n" + stepB
	f := newFixture(t)
	f.write(".templates/workflows/w.md", tmpl)
	f.write("workflows/w.md", deployed)
	ff := findFile(f.compare(), "workflows/w.md")
	unrelated := ApprovedChange{PreApply: []byte("x\ny\n"), PostApply: []byte("x\n")}
	got := ff.ExemptHunks([]ApprovedChange{unrelated, {PreApply: []byte(tmpl), PostApply: []byte(deployed)}})
	if !got[0] {
		t.Fatalf("union failed: %v", got)
	}
}

// Round 15 F4: in exact mode, a tuner-moved block (the template still has it,
// elsewhere) is tuning, not "removed by the template".
func TestExactMode_AMovedBlockIsNotATemplateRemoval(t *testing.T) {
	f := newFixture(t)
	tmpl := "steps:\n" + stepA + stepB
	f.origin(tmpl, "r0", "created")
	f.write(".templates/workflows/w.md", tmpl)
	f.write("workflows/w.md", "steps:\n"+stepB+stepA)
	ff := findFile(f.compare(), "workflows/w.md")
	if ff != nil && len(ff.RemovedByTemplate) > 0 {
		t.Fatalf("a moved block was reported as removed by the template: %v", ff.RemovedByTemplate)
	}
}
