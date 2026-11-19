package forge

import (
	"strings"
	"testing"
)

func file801(p string) string {
	return "diff --git a/" + p + " b/" + p + "\n--- a/" + p + "\n+++ b/" + p + "\n@@ -0,0 +1 @@\n+" + strings.Repeat("x", 50) + "\n"
}

func TestDiffParts801_CodeFirstAndNamedRest(t *testing.T) {
	diff := file801("go.sum") + file801("README.md") + file801("app/a_test.go") + file801("app/a.go")
	kept, notIn := inlineDiff([]byte(diff), 1<<20)
	order := []string{"app/a.go", "app/a_test.go", "README.md", "go.sum"}
	last := -1
	for _, p := range order {
		i := strings.Index(string(kept), "diff --git a/"+p)
		if i < 0 || i < last {
			t.Fatalf("%s out of code-first order in:\n%s", p, kept)
		}
		last = i
	}
	if len(notIn) != 0 {
		t.Fatalf("notIn = %v with room for all", notIn)
	}
	// A budget that fits the code file only names the rest.
	codeLen := len(file801("app/a.go"))
	kept, notIn = inlineDiff([]byte(diff), codeLen+10)
	if !strings.Contains(string(kept), "app/a.go") || strings.Contains(string(kept), "go.sum") {
		t.Fatalf("kept = %s", kept)
	}
	if strings.Join(notIn, ",") != "app/a_test.go,README.md,go.sum" {
		t.Fatalf("notIn = %v", notIn)
	}
}
