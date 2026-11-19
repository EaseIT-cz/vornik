package forge

import "testing"

func TestAddedLines801_HunksRenamesDeletions(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\nindex 1..2 100644\n--- a/a.go\n+++ b/a.go\n" +
		"@@ -1,3 +1,4 @@\n package a\n+// added at 2\n func x() {}\n-func y() {}\n+func z() {}\n" +
		"@@ -10,2 +11,3 @@ func q() {\n ctx\n+// added at 12\n ctx\n" +
		"diff --git a/old.go b/new.go\nsimilarity index 90%\nrename from old.go\nrename to new.go\n--- a/old.go\n+++ b/new.go\n@@ -5 +5,2 @@\n kept\n+// added at 6\n" +
		"diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n--- a/gone.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-x\n-y\n" +
		"diff --git a/n.txt b/n.txt\nnew file mode 100644\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1,2 @@\n+one\n+two\n\\ No newline at end of file\n"
	a, _ := parseAddedLines([]byte(diff))
	for _, c := range []struct {
		file string
		line int
		want bool
	}{
		{"a.go", 2, true}, {"a.go", 4, true}, {"a.go", 1, false}, {"a.go", 3, false}, {"a.go", 12, true}, {"a.go", 11, false},
		{"new.go", 6, true}, {"old.go", 6, false},
		{"gone.go", 1, false},
		{"n.txt", 1, true}, {"n.txt", 2, true}, {"n.txt", 3, false},
		{"./a.go", 2, true},
	} {
		if got := a.has(c.file, c.line); got != c.want {
			t.Errorf("%s:%d = %v, want %v", c.file, c.line, got, c.want)
		}
	}
}

// review-20261118-befa F2: an added line that looks like a header is
// content, and a header that does not parse is counted.
func TestAddedLines801_HeaderLookalikesAreContent(t *testing.T) {
	diff := "diff --git a/f.patch b/f.patch\n--- a/f.patch\n+++ b/f.patch\n@@ -1,1 +1,4 @@\n x\n+++ b/inner.go\n+@@ -1 +1 @@\n+after\n" +
		"diff --git a/g.go b/g.go\n--- a/g.go\n+++ b/g.go\n@@ broken @@\n+z\n"
	a, anomalies := parseAddedLines([]byte(diff))
	for _, line := range []int{2, 3, 4} {
		if !a.has("f.patch", line) {
			t.Errorf("f.patch:%d lost after a header lookalike", line)
		}
	}
	if a.has("inner.go", 1) || anomalies != 1 || a.has("g.go", 1) {
		t.Fatalf("inner %v anomalies %d g %v", a.has("inner.go", 1), anomalies, a.has("g.go", 1))
	}
}
