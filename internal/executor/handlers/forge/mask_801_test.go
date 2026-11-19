package forge

// GitHub #801 B (github-app-channel design, "A review good by default"):
// the diff is masked before any model, and the mask is proven by a rescan.

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/secrets"
)

func detector801(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{Patterns: secrets.DefaultPatterns()})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const diff801 = `diff --git a/app/config.go b/app/config.go
index 3f2a9c1d8e7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f..9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d 100644
--- a/app/config.go
+++ b/app/config.go
@@ -1,3 +1,4 @@
 package app
+const key = "AKIAIOSFODNN7EXAMPLE"
+var aws = "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
 func f() {}
`

func TestMask801_MasksProvesAndCountsNeverValues(t *testing.T) {
	masked, counts, err := maskDiff(detector801(t), []byte(diff801))
	if err != nil {
		t.Fatal(err)
	}
	out := string(masked)
	for _, leak := range []string{"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG"} {
		if strings.Contains(out, leak) {
			t.Errorf("the masked diff still holds %q", leak)
		}
	}
	if !strings.Contains(out, "[REDACTED:") {
		t.Error("no redaction marker")
	}
	// The index line's blob hashes are not masked: header lines are exempt
	// from entropy findings.
	if !strings.Contains(out, "index 3f2a9c1d8e7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f..") {
		t.Errorf("a header line was masked:\n%s", out)
	}
	total := 0
	for typ, n := range counts {
		total += n
		if strings.Contains(typ, "AKIA") {
			t.Error("a count key carries a value")
		}
	}
	if total < 2 {
		t.Errorf("counts = %v, want at least the two planted values", counts)
	}
}

// A proof that fails is a failed step: nothing reaches a model.
func TestMask801_AFailedProofFailsClosed(t *testing.T) {
	// A finding the redactor cannot apply leaves the value in place, so the
	// rescan still sees it.
	d := proofBreaker{}
	if _, _, err := maskDiff(d, []byte(diff801)); err == nil || !strings.Contains(err.Error(), "did not hold") {
		t.Fatalf("a mask that did not hold = %v, want an error", err)
	}
	if _, _, err := maskDiff(nil, []byte(diff801)); err == nil || !strings.Contains(err.Error(), "secrets") {
		t.Fatalf("no detector = %v, want a refusal naming secrets", err)
	}
}

// proofBreaker reports a finding on the first pass that Redact cannot apply
// (an out-of-range span), and the same text on the second: the proof sees
// the value still there.
type proofBreaker struct{}

func (proofBreaker) Scan(text []byte) []secrets.Finding {
	if i := strings.Index(string(text), "AKIA"); i >= 0 {
		return []secrets.Finding{{Type: "aws", Start: i, End: len(text) + 10}}
	}
	return nil
}

// review-20261118-4c70 F2: a value in a hunk header's trailing text is
// masked; only an exact index line is exempt.
func TestMask801_AHunkHeaderValueIsMasked(t *testing.T) {
	diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,2 @@ const k = \"AKIAIOSFODNN7EXAMPLE\"\n ctx\n+y\n"
	masked, _, err := maskDiff(detector801(t), []byte(diff))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(masked), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("a hunk header kept its value:\n%s", masked)
	}
	if !onIndexLine([]byte("index abc1234..def5678 100644"), 3) || onIndexLine([]byte("index abc1234..def5678 secret=x"), 3) {
		t.Fatal("the index-line rule is not exact")
	}
}

// Only an entropy finding is exempt on an index line; a pattern finding
// there is masked (review-20261118-8458 suggestion 1).
func TestMask801_APatternOnAnIndexLineIsMasked(t *testing.T) {
	fake := patternOnIndex{}
	masked, _, err := maskDiff(fake, []byte("index abc1234..def5678 100644\n+x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(masked), "[REDACTED:aws]") {
		t.Fatalf("a pattern finding on an index line was exempt:\n%s", masked)
	}
}

type patternOnIndex struct{}

func (patternOnIndex) Scan(text []byte) []secrets.Finding {
	if i := strings.Index(string(text), "abc1234"); i >= 0 {
		return []secrets.Finding{{Type: "aws", Start: i, End: i + 7}}
	}
	return nil
}
