package text

// GitHub #633 (reviewable-output design §6.3): text.readability measures the
// text a review is about and stores the report as a task OUTPUT artifact,
// with no model in between.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

type fakeArtifacts struct {
	arts    map[string]*persistence.Artifact
	bodies  map[string]string
	stored  map[string]string
	storeTo string // project the last Store was called with
}

func newFake() *fakeArtifacts {
	return &fakeArtifacts{arts: map[string]*persistence.Artifact{}, bodies: map[string]string{}, stored: map[string]string{}}
}

func (f *fakeArtifacts) add(id, project, name, mime, body string) {
	a := &persistence.Artifact{ID: id, ProjectID: project, Name: name}
	if mime != "" {
		a.MimeType = &mime
	}
	f.arts[id] = a
	f.bodies[id] = body
}

func (f *fakeArtifacts) Get(_ context.Context, id string) (*persistence.Artifact, error) {
	a, ok := f.arts[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return a, nil
}

func (f *fakeArtifacts) Open(_ context.Context, id string) (io.ReadCloser, error) {
	b, ok := f.bodies[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(strings.NewReader(b)), nil
}

func (f *fakeArtifacts) Store(_ context.Context, projectID, _, _, name, sourcePath string) (*persistence.Artifact, error) {
	b, err := os.ReadFile(sourcePath)
	if err != nil {
		return nil, err
	}
	f.stored[name] = string(b)
	f.storeTo = projectID
	return &persistence.Artifact{ID: "artifact_out", Name: name}, nil
}

func taskWithInputs(ids ...string) *persistence.Task {
	payload, _ := json.Marshal(map[string]any{"context": map[string]any{"inputArtifactIDs": ids}})
	return &persistence.Task{ID: "task_1", ProjectID: "p1", Payload: payload}
}

func run(t *testing.T, h *ReadabilityHandler, in executor.SystemStepInput) (map[string]any, error) {
	t.Helper()
	if in.Execution == nil {
		in.Execution = &persistence.Execution{ID: "exec_1"}
	}
	res, err := h.Execute(context.Background(), in)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal(res.Result, &out))
	return out, nil
}

func TestReadability_Name(t *testing.T) {
	require.Equal(t, "text.readability", NewReadabilityHandler(newFake(), newFake()).Name())
}

func TestReadability_MeasuresStagedInputsAndStoresTheReport(t *testing.T) {
	f := newFake()
	f.add("a1", "p1", "design.md", "text/markdown", "This generally works. See above.\n")
	h := NewReadabilityHandler(f, f)
	out, err := run(t, h, executor.SystemStepInput{Task: taskWithInputs("a1")})
	require.NoError(t, err)
	report := f.stored[ReportName]
	require.Contains(t, report, "| design.md |")
	require.Contains(t, report, "design.md:1")
	require.Equal(t, "p1", f.storeTo)
	require.Equal(t, report, out["message"], "the same text reaches the next agent")
	require.Equal(t, "artifact_out", out["artifact_id"])
	require.EqualValues(t, 1, out["files_examined"])
}

func TestReadability_RefusesAnotherProjectsArtifact(t *testing.T) {
	f := newFake()
	f.add("a1", "other", "design.md", "text/markdown", "Fine.\n")
	_, err := run(t, NewReadabilityHandler(f, f), executor.SystemStepInput{Task: taskWithInputs("a1")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "another project")
	require.Empty(t, f.stored)
}

func TestReadability_NonTextAndOversizeListedAsNotExamined(t *testing.T) {
	f := newFake()
	f.add("a1", "p1", "scan.pdf", "application/pdf", "%PDF")
	f.add("a2", "p1", "big.md", "", strings.Repeat("word ", maxInputBytes/5+10))
	f.add("a3", "p1", "notes.txt", "", "Fine.\n")
	out, err := run(t, NewReadabilityHandler(f, f), executor.SystemStepInput{Task: taskWithInputs("a1", "a2", "a3")})
	require.NoError(t, err)
	report := f.stored[ReportName]
	require.Contains(t, report, "scan.pdf: not a text file")
	require.Contains(t, report, "big.md: larger than 1 MiB")
	require.Contains(t, report, "| notes.txt |")
	require.EqualValues(t, 1, out["files_examined"])
	require.EqualValues(t, 2, out["files_not_examined"])
}

func TestReadability_DiffMeasuresAddedProseLinesAtTheirLineNumbers(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/docs/guide.md b/docs/guide.md",
		"--- a/docs/guide.md",
		"+++ b/docs/guide.md",
		"@@ -10,2 +10,3 @@",
		" Unchanged line probably.",
		"-Removed line probably.",
		"+Added line that generally works.",
		"+Second added line.",
		"diff --git a/main.go b/main.go",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1 +1,2 @@",
		"+// this probably panics",
		"",
	}, "\n")
	prev, _ := json.Marshal(map[string]any{"message": diff, "diff": diff})
	f := newFake()
	out, err := run(t, NewReadabilityHandler(f, f), executor.SystemStepInput{
		Task: &persistence.Task{ID: "task_1", ProjectID: "p1"}, PrevResult: prev,
	})
	require.NoError(t, err)
	report := f.stored[ReportName]
	require.Contains(t, report, "| docs/guide.md |")
	require.Contains(t, report, "docs/guide.md:11", "the added line keeps its new-file line number")
	require.Equal(t, 1, strings.Count(report, " hedge: "), "only the added prose line: not context, removed or code lines")
	require.NotContains(t, report, "main.go |")
	require.EqualValues(t, 1, out["files_examined"])
}

func TestReadability_NothingToMeasureStillSucceeds(t *testing.T) {
	f := newFake()
	out, err := run(t, NewReadabilityHandler(f, f), executor.SystemStepInput{Task: &persistence.Task{ID: "task_1", ProjectID: "p1"}})
	require.NoError(t, err)
	require.Contains(t, out["message"], "Files examined: 0")
}

func TestReadability_StoreFailureIsAnError(t *testing.T) {
	f := newFake()
	f.add("a1", "p1", "design.md", "", "Fine.\n")
	h := NewReadabilityHandler(f, failingStore{})
	_, err := run(t, h, executor.SystemStepInput{Task: taskWithInputs("a1")})
	require.Error(t, err)
}

type failingStore struct{}

func (failingStore) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (failingStore) Store(context.Context, string, string, string, string, string) (*persistence.Artifact, error) {
	return nil, errors.New("disk full")
}

func TestReadability_MissingDependenciesAndTask(t *testing.T) {
	_, err := (&ReadabilityHandler{}).Execute(context.Background(), executor.SystemStepInput{})
	require.Error(t, err)
	f := newFake()
	_, err = NewReadabilityHandler(f, f).Execute(context.Background(), executor.SystemStepInput{})
	require.Error(t, err)
}

func TestReadability_UnreadableArtifactListedAsNotExamined(t *testing.T) {
	f := newFake()
	f.add("a1", "p1", "notes.md", "", "Fine.\n")
	delete(f.bodies, "a1")
	out, err := run(t, NewReadabilityHandler(f, f), executor.SystemStepInput{Task: taskWithInputs("a1")})
	require.NoError(t, err)
	require.Contains(t, f.stored[ReportName], "notes.md: could not be read")
	require.EqualValues(t, 1, out["files_not_examined"])
}

func TestReadability_MalformedPreviousResultMeasuresNothing(t *testing.T) {
	require.Empty(t, diffFrom(json.RawMessage(`not json`)))
	require.Empty(t, diffFrom(nil))
	files, skip := measureDiff("")
	require.Empty(t, files)
	require.Empty(t, skip)
	// A deleted prose file and the no-newline marker add nothing.
	files, _ = measureDiff("--- a/x.md\n+++ /dev/null\n@@ -1 +0,0 @@\n-gone\n\\ No newline at end of file\n")
	require.Empty(t, files)
}

// The double answers a miss the way production does (repository miss
// contract): ErrNotFound, not (nil, nil).
func TestReadabilityDoubles_MissContract(t *testing.T) {
	repotest.AssertMiss(t, "ArtifactRepository.Get", func() (*persistence.Artifact, error) {
		return newFake().Get(context.Background(), "artifact-absent")
	})
}

// Security review of 75a87da36 and c8954a027: a pull request author controls
// every hunk header. Diff mode used to pad added lines with blanks up to the
// claimed line number, so "@@ -1 +900000000 @@" asked for a 900-million-line
// buffer, and many files each claiming line 99,999 multiplied the work. The
// added lines are now measured as they are, keeping their numbers.
func TestReadability_HostileHunkLineNumberCostsNothing(t *testing.T) {
	diff := "+++ b/docs/a.md\n@@ -1 +900000000 @@\n+This probably works.\n" +
		"+++ b/docs/b.md\n@@ -1 +3 @@\n+Fine here.\n"
	files, skip := measureDiff(diff)
	require.Empty(t, skip)
	require.Len(t, files, 2)
	require.Equal(t, 900000000, files[0].Report.Hedges[0].Line, "the claimed number is kept, not allocated")
}

func TestReadability_ManyFilesWithHighLineNumbersStayCheap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "+++ b/docs/f%d.md\n@@ -1 +99999 @@\n+Fine.\n", i)
	}
	start := time.Now()
	files, _ := measureDiff(b.String())
	require.Len(t, files, 5000)
	require.Less(t, time.Since(start), 5*time.Second, "work grows with the diff, not the claimed line numbers")
}

// The diff itself is bounded like a staged file: only the first maxInputBytes
// are measured, and the report says the rest was not examined.
func TestReadability_OversizeDiffIsCut(t *testing.T) {
	line := "+" + strings.Repeat("word ", 2000) + "\n" // ~10 KB, so ~110 lines pass 1 MiB
	big := "+++ b/docs/a.md\n@@ -1 +1 @@\n" + strings.Repeat(line, maxInputBytes/len(line)+10)
	files, skip := measureDiff(big)
	require.Len(t, files, 1)
	require.NotEmpty(t, skip)
	require.Contains(t, skip[len(skip)-1].Reason, "diff beyond 1 MiB")
}
