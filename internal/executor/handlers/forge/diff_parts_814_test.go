package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/registry"
)

// GitHub #814 (github-app-channel design, "Reading a large diff in parts"):
// the files past the inline budget are served in parts, computed from the
// masked diff alone so the fetch that counts them and the reader that serves
// them agree.

func fileN814(p string, lines int) string {
	var b strings.Builder
	b.WriteString("diff --git a/" + p + " b/" + p + "\n--- a/" + p + "\n+++ b/" + p + "\n@@ -0,0 +1," + strconv.Itoa(lines) + " @@\n")
	for i := 0; i < lines; i++ {
		b.WriteString("+" + strings.Repeat("y", 39) + "\n") // 41 bytes a line
	}
	return b.String()
}

func TestDiffParts814_InlineFirstThenWholeFilesInOrder(t *testing.T) {
	diff := []byte(fileN814("app/a.go", 10) + fileN814("app/b.go", 10) + fileN814("docs/c.md", 10) + fileN814("go.sum", 10))
	one := len(fileN814("app/a.go", 10))
	parts := diffParts(diff, one+1, 2*one+10)

	kept, notIn := inlineDiff(diff, one+1)
	if !bytes.Equal(parts[0], kept) {
		t.Fatalf("part 1 must be the inline text")
	}
	if len(parts) != 3 {
		t.Fatalf("want inline + 2 parts, got %d", len(parts))
	}
	// The rest follow not_in_diff's order, whole files packed to the budget.
	var got []string
	for _, p := range parts[1:] {
		for _, f := range splitDiffFiles(p) {
			got = append(got, f.path)
		}
	}
	if strings.Join(got, ",") != strings.Join(notIn, ",") {
		t.Fatalf("parts order %v, not_in_diff %v", got, notIn)
	}
	for i, p := range parts[1:] {
		if len(p) > 2*one+10 {
			t.Errorf("part %d is %d bytes, over the budget", i+2, len(p))
		}
	}
}

func TestDiffParts814_LargeFileIsCutAtLineEndsWithItsHeader(t *testing.T) {
	big := fileN814("app/big.go", 100) // ~4.1 KiB
	diff := []byte(fileN814("app/a.go", 1) + big)
	parts := diffParts(diff, len(fileN814("app/a.go", 1))+1, 1024)
	if len(parts) < 5 {
		t.Fatalf("a 4 KiB file in 1 KiB parts needs at least 4 pieces, got %d parts", len(parts))
	}
	var body strings.Builder
	for i, p := range parts[1:] {
		if len(p) > 1024 {
			t.Errorf("part %d is %d bytes", i+2, len(p))
		}
		s := string(p)
		if !strings.HasPrefix(s, "diff --git a/app/big.go b/app/big.go\n") {
			t.Errorf("piece %d does not repeat the file header: %q", i+1, s[:40])
		}
		if !strings.HasSuffix(s, "\n") {
			t.Errorf("piece %d is not cut at a line end", i+1)
		}
		if !strings.Contains(s, "piece ") {
			t.Errorf("piece %d is not labelled", i+1)
		}
		// A piece cut inside a hunk restarts it with the new-file line its
		// first line has, so a finding there cites the right line.
		wantStart := strings.Count(body.String(), "+y") + 1
		var hunk string
		for _, l := range strings.SplitAfter(s, "\n") {
			if strings.HasPrefix(l, "@@ ") && hunk == "" {
				hunk = l
			}
		}
		if !strings.Contains(hunk, " +"+itoa814(wantStart)) {
			t.Errorf("piece %d hunk header %q, want new-file start %d", i+1, hunk, wantStart)
		}
		for _, l := range strings.SplitAfter(s, "\n") {
			if strings.HasPrefix(l, "+y") {
				body.WriteString(l)
			}
		}
	}
	want := strings.Count(big, "\n+y")
	if strings.Count(body.String(), "+y") != want {
		t.Fatalf("pieces carry %d added lines, want %d (none lost or repeated)", strings.Count(body.String(), "+y"), want)
	}
}

func TestDiffParts814_FitsInlineMeansOnePart(t *testing.T) {
	diff := []byte(fileN814("app/a.go", 3))
	if parts := diffParts(diff, 1<<20, 1024); len(parts) != 1 {
		t.Fatalf("a diff that fits inline has one part, got %d", len(parts))
	}
}

func itoa814(n int) string { return strconv.Itoa(n) }

func TestHunkStarts814_IgnoresTheSectionText(t *testing.T) {
	o, n := hunkStarts([]byte("@@ -12,3 +40,5 @@ func x(a -1, b +2) {\n"))
	if o != 12 || n != 40 {
		t.Fatalf("got -%d +%d, want -12 +40", o, n)
	}
	if o, n := hunkStarts([]byte("@@ -7 +9 @@\n")); o != 7 || n != 9 {
		t.Fatalf("short form: -%d +%d", o, n)
	}
}

// #814 F4/F5: every file is named. A quoted path is read, a header without a
// b/ path falls back to the +++ line, and a file still without one is named
// in not_in_diff instead of vanishing from the review.
func TestDiffGitPath814_QuotedAndFallback(t *testing.T) {
	quoted := "diff --git \"a/my file.go\" \"b/my file.go\"\n--- \"a/my file.go\"\n+++ \"b/my file.go\"\n@@ -0,0 +1 @@\n+x\n"
	odd := "diff --git weird\n--- a/odd.go\n+++ b/odd.go\n@@ -0,0 +1 @@\n+x\n"
	none := "diff --git weird\n@@ -0,0 +1 @@\n+x\n"
	files := splitDiffFiles([]byte(quoted + odd + none))
	if len(files) != 3 || files[0].path != "my file.go" || files[1].path != "odd.go" || files[2].path != "" {
		t.Fatalf("paths = %q %q %q", files[0].path, files[1].path, files[2].path)
	}
	_, notIn := inlineDiff([]byte(quoted+odd+none), 1)
	if strings.Join(notIn, "|") != "my file.go|odd.go|<file 3 without a path>" {
		t.Fatalf("not_in_diff = %q", notIn)
	}
}

// #814 at the handler: a masked fetch over the inline budget counts its parts
// and tells the reviewers how to read them; one that fits says nothing more.
func TestFetchDiffMask814_CountsPartsAndSaysHowToReadThem(t *testing.T) {
	ctx := context.Background()
	big := fileN814("app/a.go", 1) + fileN814("app/big.go", 6000) // ~240 KiB past the inline part
	prov := &fakeProvider{}
	prov.diff = []byte(big)
	task := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4})
	step := &registry.WorkflowStep{MaskSecrets: true}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithSecretsDetector(detector801(t))
	res, err := h.Execute(ctx, executor.SystemStepInput{Task: task, Step: step})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Message   string
		Parts     int
		NotInDiff []string `json:"not_in_diff"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatal(err)
	}
	want := len(servedParts([]byte(big)))
	if out.Parts != want || want < 3 {
		t.Fatalf("parts = %d, want %d (>= 3)", out.Parts, want)
	}
	if !strings.Contains(out.Message, "Read them with forge_diff_part, parts 2 to "+strconv.Itoa(want)+".") {
		t.Fatalf("message does not say how to read the parts: %.300s", out.Message)
	}

	prov.diff = []byte(fileN814("app/a.go", 3))
	res, _ = h.Execute(ctx, executor.SystemStepInput{Task: task, Step: step})
	if strings.Contains(string(res.Result), "forge_diff_part") || !strings.Contains(string(res.Result), `"parts":1`) {
		t.Fatalf("a diff that fits inline: %.300s", res.Result)
	}
}

// #814 §5: a not-inline file counts as reviewed only when every part holding
// it is in some step's parts_read; a file the fetch cut partway never does.
func TestNotReviewed814_CountsWholeFilesOnly(t *testing.T) {
	diff := fileN814("app/a.go", 1) + fileN814("app/b.go", 10) + fileN814("app/big.go", 6000)
	parts := servedParts([]byte(diff))
	var bigParts []int
	for i, p := range parts[1:] {
		if strings.Contains(string(p), "big.go") {
			bigParts = append(bigParts, i+2)
		}
	}
	if len(bigParts) < 2 {
		t.Fatalf("big.go should span several parts, got %v", bigParts)
	}
	_, notIn := inlineDiff([]byte(diff), diffInlineBudget)
	hunter := func(read []int) json.RawMessage {
		b, _ := json.Marshal(read)
		m, _ := json.Marshal(map[string]string{"message": "I read the parts.\n{\"findings\":[],\"parts_read\":" + string(b) + "}"})
		return m
	}
	// Nothing listed: every not-inline file is not reviewed.
	got := notReviewed(diff, notIn, "", map[string]json.RawMessage{})
	if strings.Join(got, ",") != strings.Join(notIn, ",") {
		t.Fatalf("no parts_read: %v, want %v", got, notIn)
	}
	// All but the last piece of big.go: big.go is still not reviewed.
	steps := map[string]json.RawMessage{"hunt_a": hunter(bigParts[:len(bigParts)-1])}
	if got := notReviewed(diff, notIn, "", steps); !contains814(got, "app/big.go") {
		t.Fatalf("a file with a piece unread counted reviewed: %v", got)
	}
	// Every part, across two hunters: nothing left.
	var all []int
	for n := 2; n <= len(parts); n++ {
		all = append(all, n)
	}
	steps = map[string]json.RawMessage{"hunt_a": hunter(all[:1]), "hunt_b": hunter(all[1:])}
	if got := notReviewed(diff, notIn, "", steps); len(got) != 0 {
		t.Fatalf("every part read, still not reviewed: %v", got)
	}
	// A file the fetch cut is never reviewed.
	if got := notReviewed(diff, notIn, "app/big.go", steps); !contains814(got, "app/big.go") {
		t.Fatalf("the cut file counted reviewed: %v", got)
	}
}

func contains814(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// #814 N1: a fetch cut at the read cap names the file it cut partway.
func TestFetchDiffMask814_NamesTheCutFile(t *testing.T) {
	prov := &fakeProvider{}
	prov.diff = []byte(fileN814("app/a.go", 1) + "diff --git a/app/huge.go b/app/huge.go\n--- a/app/huge.go\n+++ b/app/huge.go\n@@ -0,0 +1 @@\n+" + strings.Repeat("y", diffReadCap) + "\n")
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithSecretsDetector(detector801(t))
	res, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4}), Step: &registry.WorkflowStep{MaskSecrets: true}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Message string
		CutFile string `json:"cut_file"`
	}
	_ = json.Unmarshal(res.Result, &out)
	if out.CutFile != "app/huge.go" || !strings.Contains(out.Message, "The fetch cut app/huge.go partway.") {
		t.Fatalf("cut_file %q, message %.300q", out.CutFile, out.Message)
	}
}

// #814 §5 at the handler: the posted review names a not-inline file only
// while the hunters have not read all of its parts.
func TestPostReview814_NotReviewedFollowsPartsRead(t *testing.T) {
	diff := fileN814("app/a.go", 1) + fileN814("app/big.go", 6000)
	_, notIn := inlineDiff([]byte(diff), diffInlineBudget)
	fetched, _ := json.Marshal(map[string]any{"diff": diff, "head_sha": "abc123", "not_in_diff": notIn})
	verdict, _ := json.Marshal(map[string]any{"review": map[string]any{"approved": true, "summary": "Fine.", "findings": []any{}}})
	var all []int
	for n := 2; n <= len(servedParts([]byte(diff))); n++ {
		all = append(all, n)
	}
	post := func(read []int) string {
		prov := &checkingProvider{fakeProvider: &fakeProvider{}}
		h := newTestPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).WithSecretsDetector(detector801(t))
		b, _ := json.Marshal(read)
		hunt, _ := json.Marshal(map[string]string{"message": "{\"findings\":[],\"parts_read\":" + string(b) + "}"})
		in := executor.SystemStepInput{Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4}), PrevResult: verdict,
			Step:        &registry.WorkflowStep{GroundFindings: true},
			StepResults: map[string]json.RawMessage{"fetch_diff": fetched, "hunt_a": hunt}}
		if _, err := h.Execute(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		return prov.gotReview.Body
	}
	if body := post(nil); !strings.Contains(body, "Not reviewed: 1 files (app/big.go)") {
		t.Fatalf("nothing read, body:\n%s", body)
	}
	if body := post(all); strings.Contains(body, "Not reviewed") {
		t.Fatalf("every part read, body:\n%s", body)
	}
}

// A file with no hunk (binary, or header only) is never lost: it is served
// whole, even when it is larger than a part.
func TestCutFile814_FileWithoutHunksIsServedWhole(t *testing.T) {
	f := diffFile{path: "bin.dat", n: 1, text: []byte("diff --git a/bin.dat b/bin.dat\nindex 1111111..2222222 100644\n" + strings.Repeat("Binary files differ\n", 100))}
	pieces := cutFile(f, 256)
	if len(pieces) != 1 || !bytes.Equal(pieces[0], f.text) {
		t.Fatalf("pieces = %d, want the whole file once", len(pieces))
	}
}

// #814 review a38e L2: a "\ No newline" marker is never cut from its line.
func TestCutFile814_NoNewlineMarkerStaysWithItsLine(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -0,0 +1,40 @@\n")
	for i := 0; i < 40; i++ {
		b.WriteString("+" + strings.Repeat("y", 39) + "\n")
	}
	b.WriteString("\\ No newline at end of file\n")
	for size := 300; size < 600; size += 7 {
		for i, p := range cutFile(diffFile{path: "a.go", n: 1, text: []byte(b.String())}, size) {
			body := string(p)[strings.Index(string(p), "]\n")+2:]
			if strings.HasPrefix(strings.TrimPrefix(body, "@@ -0 +41 @@ (continued)\n"), "\\") {
				t.Fatalf("size %d: piece %d starts with the marker", size, i+1)
			}
		}
	}
}
