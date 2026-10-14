package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/registry"
)

// Tests for GitHub #351 (audit 2026-10-12, trust-zone invariant TZ-3, staging
// half): input staging copied another project's stored artifact, or another
// execution's temp file, into a project-A container. The design is
// https://docs.vornik.io §13.

// TestW35_StagingRefusesAnotherProjectsArtifact is the red probe recorded in
// GitHub #351, committed unchanged. Before the fix the roots admitted the
// whole artifact store, so project A's staging accepted project B's file.
func TestW35_StagingRefusesAnotherProjectsArtifact(t *testing.T) {
	store := t.TempDir()
	other := filepath.Join(store, "projB", "exec_1")
	_ = os.MkdirAll(other, 0o755)
	secret := filepath.Join(other, "secret.pdf")
	_ = os.WriteFile(secret, []byte("B's data"), 0o600)
	payload, _ := json.Marshal(map[string]any{"context": map[string]any{"inputFiles": []string{secret}}})
	arts := extractTaskInputArtifacts(payload, 1<<20, false, 1<<20)
	roots := allowedStagingRoots("", "projA", store)
	if _, ok := resolveStagingSrc(arts[0]["sourcePath"], roots); ok {
		t.Fatalf("project A's staging accepted project B's artifact %s (roots %v)", arts[0]["sourcePath"], roots)
	}
}

// TestStagingRoots_NoTempDirAndOnlyOwnProjectSubtree — audit 2026-10-12
// (#351, C1): the roots are the project's own store subtree and its legacy
// uploads directory, nothing else. The temp dir (every live execution's
// vornik-exec-* workspace and every depth-1 temp file, of any project) is
// gone, and an empty or unclean project id yields no roots at all.
func TestStagingRoots_NoTempDirAndOnlyOwnProjectSubtree(t *testing.T) {
	store := t.TempDir()
	ws := t.TempDir()
	roots := allowedStagingRoots(ws, "projA", store)
	require.Len(t, roots, 2, "exactly the store subtree and the uploads dir: %v", roots)

	wantStore := canonicalRoot(filepath.Join(store, "projA"))
	wantUploads := canonicalRoot(filepath.Join(ws, "projA", "uploads"))
	assert.ElementsMatch(t, []string{wantStore, wantUploads}, roots)
	tmp := canonicalRoot(os.TempDir())
	for _, r := range roots {
		assert.NotEqual(t, tmp, r, "the temp dir must not be a staging root")
	}

	// A depth-1 temp file (the shape of the forge handler's patch file) is
	// refused.
	f, err := os.CreateTemp("", "w3-11-patch-*.patch")
	require.NoError(t, err)
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	_, ok := resolveStagingSrc(f.Name(), roots)
	assert.False(t, ok, "a temp-dir file must not stage")

	// A sibling project whose id shares the prefix is outside.
	sib := filepath.Join(store, "projAB", "exec", "x.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(sib), 0o755))
	require.NoError(t, os.WriteFile(sib, []byte("x"), 0o600))
	_, ok = resolveStagingSrc(sib, roots)
	assert.False(t, ok, "projAB is not under projA")

	assert.Empty(t, allowedStagingRoots(ws, "", store), "no project, no roots")
	assert.Empty(t, allowedStagingRoots(ws, "../projB", store), "an unclean project id yields no roots")
	assert.Empty(t, allowedStagingRoots("", "projA", ""), "no store, no workspace: no roots")
}

// rowRepo is an executor ArtifactRepository that also answers Get, the shape
// of the production persistence repository.
type rowRepo struct {
	MockArtifactRepo
	rows map[string]*persistence.Artifact
}

func (r *rowRepo) Get(_ context.Context, id string) (*persistence.Artifact, error) {
	if a, ok := r.rows[id]; ok {
		return a, nil
	}
	return nil, persistence.ErrNotFound
}

// TestRowRepo_MissContract — the double answers a miss the way production
// does (ErrNotFound), so the confinement's "unknown ID" path is exercised.
func TestRowRepo_MissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "ArtifactRepository.Get", (&rowRepo{}).Get)
}

// ownRowsOfP returns project-"p" rows for the artifact IDs the
// ingestInputArtifacts tests that predate GitHub #351 use: the ingest path
// now checks each ID is a row of the task's project.
func ownRowsOfP() *rowRepo {
	rows := map[string]*persistence.Artifact{}
	for _, id := range []string{"in_a", "in_b"} {
		rows[id] = &persistence.Artifact{ID: id, ProjectID: "p"}
	}
	return &rowRepo{rows: rows}
}

func writeStoreFile(t *testing.T, store, project, rel string) string {
	t.Helper()
	p := filepath.Join(store, project, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(project+" bytes"), 0o600))
	return p
}

func inputPayload(t *testing.T, files, ids []string) []byte {
	t.Helper()
	ctx := map[string]any{"inputFiles": files}
	if ids != nil {
		ctx["inputArtifactIDs"] = ids
	}
	b, err := json.Marshal(map[string]any{"context": ctx})
	require.NoError(t, err)
	return b
}

// TestConfineTaskInputArtifacts_CallerPathsAreSelectorsAmongOwnRows — audit
// 2026-10-12 (#351, C2): a task-input path stages only when it is the storage
// path of a row named in context.inputArtifactIDs and owned by the task's
// project. The payload is caller-written on several create surfaces, so it is
// never a read instruction by itself.
func TestConfineTaskInputArtifacts_CallerPathsAreSelectorsAmongOwnRows(t *testing.T) {
	store := t.TempDir()
	pathA := writeStoreFile(t, store, "projA", "inputs/art_a/mine.pdf")
	pathB := writeStoreFile(t, store, "projB", "inputs/art_b/secret.pdf")
	repo := &rowRepo{rows: map[string]*persistence.Artifact{
		"art_a": {ID: "art_a", ProjectID: "projA", StoragePath: pathA},
		"art_b": {ID: "art_b", ProjectID: "projB", StoragePath: pathB},
	}}
	e := &Executor{artifactRepo: repo, logger: zerolog.Nop()}
	ctx := context.Background()

	cases := []struct {
		name  string
		files []string
		ids   []string
		want  []string
	}{
		{"B's path named by B's own ID", []string{pathB}, []string{"art_b"}, nil},
		{"B's path smuggled beside A's ID", []string{pathB}, []string{"art_a"}, nil},
		{"B's path with no ID at all", []string{pathB}, nil, nil},
		{"A's own row", []string{pathA}, []string{"art_a"}, []string{pathA}},
		{"A's row kept, B's dropped", []string{pathA, pathB}, []string{"art_a", "art_b"}, []string{pathA}},
		{"A's path with no ID (not a row the payload names)", []string{pathA}, nil, nil},
		{"unknown ID", []string{pathA}, []string{"art_missing"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &persistence.Task{ID: "t1", ProjectID: "projA", Payload: inputPayload(t, tc.files, tc.ids)}
			arts := extractTaskInputArtifacts(task.Payload, 0, false, 0)
			got := e.confineTaskInputArtifacts(ctx, task, arts)
			var paths []string
			for _, a := range got {
				paths = append(paths, a["sourcePath"])
			}
			assert.Equal(t, tc.want, paths)
		})
	}
}

// TestConfineTaskInputArtifacts_NoRowLookupDropsAll — audit 2026-10-12 (#351,
// C2, fail closed): with no repository able to look a row up, no task-input
// path stages.
func TestConfineTaskInputArtifacts_NoRowLookupDropsAll(t *testing.T) {
	store := t.TempDir()
	pathA := writeStoreFile(t, store, "projA", "inputs/art_a/mine.pdf")
	task := &persistence.Task{ID: "t1", ProjectID: "projA", Payload: inputPayload(t, []string{pathA}, []string{"art_a"})}
	arts := extractTaskInputArtifacts(task.Payload, 0, false, 0)
	require.Len(t, arts, 1)

	for name, e := range map[string]*Executor{
		"nil repo":         {logger: zerolog.Nop()},
		"repo without Get": {artifactRepo: NewMockArtifactRepo(), logger: zerolog.Nop()},
	} {
		assert.Empty(t, e.confineTaskInputArtifacts(context.Background(), task, arts), name)
	}
}

// TestStageInputArtifacts_CrossProjectEntryPerProducer — audit 2026-10-12
// (#351, C1/C3): every producer of staging entries other than the task
// payload (E2 a step's harvested outputs, E3 an auto-resume handoff, E4
// delegated children's outputs) is bounded at the chokepoint. An entry naming
// project B's store path never reaches a project-A workspace, whichever
// producer built it; A's own entry still stages.
func TestStageInputArtifacts_CrossProjectEntryPerProducer(t *testing.T) {
	store := t.TempDir()
	pathA := writeStoreFile(t, store, "projA", "exec_a/report.md")
	pathB := writeStoreFile(t, store, "projB", "exec_b/secret.md")
	execTmp := t.TempDir() // the shape of another execution's vornik-exec-* workspace
	tmpFile := filepath.Join(execTmp, "workspace", "artifacts", "out", "live.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(tmpFile), 0o755))
	require.NoError(t, os.WriteFile(tmpFile, []byte("live"), 0o600))

	// E3 is built by the real producer from persisted state.
	st := executionState{
		AutoResumedFrom: "exec_prev",
		StepHandoffs: map[string][]map[string]string{
			"research": {{"name": "secret.md", "sourcePath": pathB, "class": "output"}},
		},
	}
	e3 := autoResumeHandoff(st, []string{"research"})
	require.Len(t, e3, 1)

	producers := map[string][]map[string]string{
		"E2 step output":     {{"name": "secret.md", "sourcePath": pathB, "class": "output"}},
		"E2 live temp file":  {{"name": "live.md", "sourcePath": tmpFile, "class": "output"}},
		"E3 resume handoff":  e3,
		"E4 delegated child": {{"name": "abcd1234-secret.md", "sourcePath": pathB}},
	}
	roots := allowedStagingRoots("", "projA", store)
	e := &Executor{logger: zerolog.Nop()}
	for name, arts := range producers {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			require.NoError(t, e.stageInputArtifacts(ws, arts, roots))
			for _, sub := range []string{"in", "out"} {
				entries, _ := os.ReadDir(filepath.Join(ws, "artifacts", sub))
				assert.Empty(t, entries, "nothing of another project may stage into artifacts/%s", sub)
			}
		})
	}

	ws := t.TempDir()
	own := []map[string]string{{"name": "report.md", "sourcePath": pathA, "class": "output"}}
	require.NoError(t, e.stageInputArtifacts(ws, own, roots))
	b, err := os.ReadFile(filepath.Join(ws, "artifacts", "out", "report.md"))
	require.NoError(t, err)
	assert.Equal(t, "projA bytes", string(b))
}

// TestIngestInputArtifacts_SkipsAnotherProjectsArtifact — audit 2026-10-12
// (#351, C4): ingestInputArtifacts enqueued every caller-written
// inputArtifactIDs entry under the task's project, so project B's artifact
// could be ingested into project A's memory. A row of another project, an
// unknown ID, or no way to look rows up, enqueues nothing for that ID.
func TestIngestInputArtifacts_SkipsAnotherProjectsArtifact(t *testing.T) {
	q := &stubIngestQueue{}
	e := &Executor{
		memoryIndexer: &stubMemoryIndexer{},
		ingestQueue:   q,
		artifactRepo: &rowRepo{rows: map[string]*persistence.Artifact{
			"in_a": {ID: "in_a", ProjectID: "projA"},
			"in_b": {ID: "in_b", ProjectID: "projB"},
		}},
		workflows: &MockWorkflowResolver{workflows: map[string]*registry.Workflow{
			"companion-rag-ingest": {ID: "companion-rag-ingest", IngestInputArtifacts: true},
		}},
		logger: zerolog.Nop(),
	}
	task := &persistence.Task{
		ID: "t", ProjectID: "projA",
		Payload: ingestInputPayload(t, []string{"in_b", "in_a", "in_missing"},
			[]string{"/s/b.md", "/s/a.md", "/s/m.md"}, ""),
	}
	e.ingestInputArtifacts(context.Background(), task, &persistence.Execution{ID: "x", WorkflowID: "companion-rag-ingest"})
	require.Len(t, q.items, 1, "only project A's own row is ingested")
	assert.Equal(t, "in_a", q.items[0].SourceArtifactID)

	q.items = nil
	e.artifactRepo = NewMockArtifactRepo() // cannot look rows up: fail closed
	e.ingestInputArtifacts(context.Background(), task, &persistence.Execution{ID: "x", WorkflowID: "companion-rag-ingest"})
	assert.Empty(t, q.items)
}
