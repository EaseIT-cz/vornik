package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/persistence/repotest"
)

// Tests for GitHub #351 (audit 2026-10-12), chat entry path
// (15-artifact-store.md §13.2 C5). create_task snapshots each input_files
// entry into a NEW row of the chat's project, so a model (prompt-injected or
// not) that names another project's stored file launders those bytes into
// an own-project row, past every later row-ownership check.

// baseStore is a confineStubStore that also exposes the artifact store's
// base path, as the production *artifacts.Store does.
type baseStore struct {
	confineStubStore
	base    string
	failErr error
}

func (s *baseStore) BasePath() string { return s.base }

func (s *baseStore) StoreInput(ctx context.Context, projectID, name, sourcePath string) (*persistence.Artifact, error) {
	if s.failErr != nil {
		return nil, s.failErr
	}
	return s.confineStubStore.StoreInput(ctx, projectID, name, sourcePath)
}

func createWithInputs(t *testing.T, te *ToolExecutor, inputs ...string) ToolResult {
	t.Helper()
	args, _ := json.Marshal(map[string]any{
		"project_id": "snake", "type": "feature", "prompt": "summarise", "input_files": inputs,
	})
	return te.createTask(context.Background(), string(args), "snake", []string{"snake"}, 0)
}

// TestCreateTask_InputFiles_RefusesAnotherProjectsStoredFile — a literal path
// into another project's store subtree is refused; the project's own stored
// file still passes. Before the fix the whole store base was a root.
func TestCreateTask_InputFiles_RefusesAnotherProjectsStoredFile(t *testing.T) {
	base := t.TempDir()
	mk := func(project string) string {
		p := filepath.Join(base, project, "exec_1", "report.md")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(project), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	other, own := mk("otherproj"), mk("snake")

	store := &baseStore{base: base}
	te := &ToolExecutor{
		registry:      confineRegistry(t),
		taskRepo:      &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}},
		artifactStore: store,
		logger:        zerolog.Nop(),
	}

	res := createWithInputs(t, te, other)
	if len(store.sources) != 0 {
		t.Fatalf("another project's stored file reached StoreInput: %v", store.sources)
	}
	if !strings.Contains(res.Content, "not an allowed input") {
		t.Errorf("expected a refusal, got %q", res.Content)
	}

	createWithInputs(t, te, own)
	if len(store.sources) != 1 {
		t.Fatalf("the project's own stored file must still pass, got %v", store.sources)
	}
}

// TestCreateTask_InputFiles_RefusesTempDirFile — the temp dir is no longer a
// default root: it holds every live execution's workspace and depth-1 temp
// files (for example the forge handler's patch file) of any project.
func TestCreateTask_InputFiles_RefusesTempDirFile(t *testing.T) {
	f, err := os.CreateTemp("", "w3-11-vornik-patch-*.patch")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(f.Name()) })

	store := &baseStore{base: t.TempDir()}
	te := &ToolExecutor{
		registry:      confineRegistry(t),
		taskRepo:      &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}},
		artifactStore: store,
		logger:        zerolog.Nop(),
	}
	createWithInputs(t, te, f.Name())
	if len(store.sources) != 0 {
		t.Fatalf("a temp-dir file reached StoreInput: %v", store.sources)
	}
	for _, r := range te.inputFileSourceRoots("snake") {
		if r == resolveRootForContainment(os.TempDir()) {
			t.Fatalf("the temp dir is still a default root: %v", te.inputFileSourceRoots("snake"))
		}
	}
}

// TestCreateTask_InputFiles_RefusesAnotherProjectsArtifactID — an artifact ID
// resolved to its row's storage path with no project check, and the result
// was trusted past the root gate. Another project's row must be refused.
func TestCreateTask_InputFiles_RefusesAnotherProjectsArtifactID(t *testing.T) {
	repo := &stubArtifactRepoForResolve{byID: map[string]*persistence.Artifact{
		"art_other": {ID: "art_other", ProjectID: "otherproj", Name: "secret.pdf",
			StoragePath: "/var/lib/vornik/artifacts/otherproj/exec/secret.pdf"},
	}}
	store := &baseStore{base: t.TempDir()}
	te := &ToolExecutor{
		registry:      confineRegistry(t),
		taskRepo:      &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}},
		artifactRepo:  repo,
		artifactStore: store,
		logger:        zerolog.Nop(),
	}
	res := createWithInputs(t, te, "art_other")
	if len(store.sources) != 0 {
		t.Fatalf("another project's artifact ID reached StoreInput: %v", store.sources)
	}
	if !strings.Contains(res.Content, "Cannot create task") {
		t.Errorf("expected a refusal, got %q", res.Content)
	}
}

// TestCreateTask_InputFiles_SnapshotFailureRefuses — a StoreInput failure
// used to pass the raw host path on to the task payload; the executor now
// stages only paths that name an own-project row, so that input would vanish
// silently. The create_task call must fail visibly instead.
func TestCreateTask_InputFiles_SnapshotFailureRefuses(t *testing.T) {
	base := t.TempDir()
	own := filepath.Join(base, "snake", "exec_1", "report.md")
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{
		registry:      confineRegistry(t),
		taskRepo:      repo,
		artifactStore: &baseStore{base: base, failErr: errors.New("disk full")},
		logger:        zerolog.Nop(),
	}
	res := createWithInputs(t, te, own)
	if repo.last != nil {
		t.Fatalf("a task was created although its input could not be snapshotted")
	}
	if !strings.Contains(res.Content, "Cannot create task") {
		t.Errorf("expected a refusal, got %q", res.Content)
	}
}

// flakyOwnerRepo resolves an ID on the first Get and fails every later one,
// so the owner check runs against a lookup error.
type flakyOwnerRepo struct {
	stubArtifactRepoForResolve
	row   *persistence.Artifact
	calls int
}

func (r *flakyOwnerRepo) Get(_ context.Context, id string) (*persistence.Artifact, error) {
	if r.row == nil || id != r.row.ID {
		return nil, persistence.ErrNotFound
	}
	r.calls++
	if r.calls == 1 {
		return r.row, nil
	}
	return nil, errors.New("db unavailable")
}

// TestCreateTask_InputFiles_OwnerLookupErrorRefuses — audit 2026-10-12 (#351,
// design review round 2 N2): when the owner of an artifact-ID entry cannot be
// read, the entry is refused; an unreadable owner is never taken to be the
// chat's project.
func TestCreateTask_InputFiles_OwnerLookupErrorRefuses(t *testing.T) {
	repo := &flakyOwnerRepo{row: &persistence.Artifact{ID: "art_x", ProjectID: "snake",
		StoragePath: "/var/lib/vornik/artifacts/snake/inputs/x/a.pdf"}}
	store := &baseStore{base: t.TempDir()}
	te := &ToolExecutor{
		registry:      confineRegistry(t),
		taskRepo:      &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}},
		artifactRepo:  repo,
		artifactStore: store,
		logger:        zerolog.Nop(),
	}
	res := createWithInputs(t, te, "art_x")
	if len(store.sources) != 0 {
		t.Fatalf("an entry whose owner could not be read reached StoreInput: %v", store.sources)
	}
	if !strings.Contains(res.Content, "Cannot create task") {
		t.Errorf("expected a refusal, got %q", res.Content)
	}
}

// TestFlakyOwnerRepo_MissContract — the double answers an unknown ID the way
// production does.
func TestFlakyOwnerRepo_MissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "ArtifactRepository.Get", (&flakyOwnerRepo{}).Get)
	repotest.AssertMissRepo(t, "ArtifactRepository.Get", (&stubArtifactRepoForResolve{}).Get)
}
