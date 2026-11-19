package forge

import (
	"strings"
	"testing"
)

func TestGround801_DropsUngroundedAndEchoed(t *testing.T) {
	added, _ := parseAddedLines([]byte("diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,3 @@\n x\n+y\n+z\n"))
	fs := []reviewFinding{
		{File: "a.go", Line: 2, Severity: "blocker", Title: "nil deref", Detail: "y may be nil"},
		{File: "a.go", Line: 1, Severity: "major", Title: "context line", Detail: "not added"},
		{File: "b.go", Line: 2, Severity: "minor", Title: "other file", Detail: "not in the diff"},
		{File: "a.go", Line: 3, Severity: "major", Title: "leak", Detail: "the key AKIAIOSFODNN7EXAMPLE is exposed"},
	}
	g := groundFindings(fs, added, detector801(t))
	if len(g.Kept) != 1 || g.Kept[0].Title != "nil deref" || g.Ungrounded != 2 || g.Echoed != 1 {
		t.Fatalf("ground = %+v", g)
	}
	body := renderGrounded("One real problem.", g, []string{"go.sum"})
	for _, want := range []string{"`a.go:2` **nil deref**", "dropped 2 findings that cited lines outside the diff", "dropped 1 findings whose text held", "Not reviewed: 1 files (go.sum)"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "AKIA") {
		t.Fatal("the body echoes the secret")
	}
	if got := groundFindings(fs[:1], added, nil); len(got.Kept) != 0 {
		t.Fatal("with no detector a finding went out unscanned")
	}
}

func TestCheckRun801_NeutralByDefaultAndCapped(t *testing.T) {
	blocker := []reviewFinding{{File: "a.go", Line: 2, Severity: "blocker", Title: "t"}}
	if checkRunConclusion("", blocker) != "neutral" || checkRunConclusion("from_verdict", blocker) != "failure" || checkRunConclusion("from_verdict", nil) != "success" {
		t.Fatal("conclusion rule")
	}
	var many []reviewFinding
	for i := 0; i < 60; i++ {
		many = append(many, reviewFinding{File: "a.go", Line: i + 1, Severity: "minor"})
	}
	if n := len(checkRunAnnotations(many)); n != checkRunAnnotationCap {
		t.Fatalf("annotations = %d, want the cap %d", n, checkRunAnnotationCap)
	}
	if a := checkRunAnnotations(blocker)[0]; a["annotation_level"] != "failure" || a["path"] != "a.go" {
		t.Fatalf("annotation = %v", a)
	}
	if checkRunTitle(blocker) != "1 findings (1 blockers)" {
		t.Fatalf("title = %q", checkRunTitle(blocker))
	}
}
