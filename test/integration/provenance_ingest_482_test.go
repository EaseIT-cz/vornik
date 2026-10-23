//go:build integration
// +build integration

package integration_test

// GitHub #482, design 22 "Amendment 2026-10-21 (#482): a companion-delegated
// chunk is verified only by provenance".
//
// Found at: review (the #435 lane's open decision 1; review 5caa F1). A user
// observes it where a companion delegates companion-rag-ingest or a review
// workflow: the task's chunks come back from recall as verified, the
// producer's role recorded as their validator. Regression test at: the real
// ingest worker loop (enqueue, drain, pipeline, indexer, repository) on the
// pgvector database, with real tasks, real artifact files and a real git
// clone in the project's workspace: the level the ingest of a delegated task
// runs at. Before the fix every companion-delegated case below landed
// validation_status='verified' with validator_role = the producer role.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"vornik.io/vornik/internal/testutil/testenv"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryscope"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/spawn"
)

func gh482Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = testenv.Clean("GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func gh482Write(t *testing.T, dir, rel, body string) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

type gh482Harness struct {
	db        *sql.DB
	project   string
	scope     string
	clone     string
	files     string
	tasks     *postgres.TaskRepository
	arts      *postgres.ArtifactRepository
	queue     *postgres.IngestQueueRepository
	verify    *persistence.SQLMemoryVerificationRepository
	newWorker func() *memory.IngestWorker
	enqueued  []string
}

func newGH482Harness(t *testing.T) *gh482Harness {
	t.Helper()
	db := connectDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := &gh482Harness{db: db, project: fmt.Sprintf("itest-gh482-%d", time.Now().UnixNano())}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM project_memory_chunks WHERE project_id = $1`,
			`DELETE FROM project_ingest_queue WHERE project_id = $1`,
			`DELETE FROM memory_chunk_provenance WHERE project_id = $1`,
			`DELETE FROM artifacts WHERE project_id = $1`,
			`DELETE FROM tasks WHERE project_id = $1`,
		} {
			_, _ = db.Exec(q, h.project)
		}
	})

	// The project's own clone: workspaces/<project>, cloned from an
	// upstream whose path is, normalised, the repo scope.
	base := t.TempDir()
	root := filepath.Join(base, "workspaces")
	upstream := filepath.Join(base, "upstream")
	require.NoError(t, os.MkdirAll(upstream, 0o755))
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, spawn.RegisterWorkspaceRoot(root))
	gh482Git(t, upstream, "init", "-q", "-b", "main")
	gh482Write(t, upstream, memory.ProvenanceLLDDir+"/22-design.md", h.body("merged"))
	gh482Write(t, upstream, "docs/plans/plan.md", h.body("plan"))
	gh482Git(t, upstream, "add", "-A")
	gh482Git(t, upstream, "commit", "-q", "-m", "init")
	gh482Git(t, root, "clone", "-q", upstream, h.project)
	h.clone = filepath.Join(root, h.project)
	h.scope = memoryscope.NormalizeRemote(upstream)
	h.files = filepath.Join(base, "artifacts")

	h.tasks = postgres.NewTaskRepository(db)
	h.arts = postgres.NewArtifactRepository(db)
	h.queue = postgres.NewIngestQueueRepository(db)
	h.verify = postgres.NewMemoryVerificationRepository(db)

	repo := memory.NewRepository(db)
	cfg := memory.DefaultConfig()
	cfg.Enabled = true
	cfg.EmbeddingEndpoint = ""
	indexer := memory.NewIndexer(cfg, repo, nil, zerolog.Nop())
	pipeline := memory.NewPipeline(indexer, memory.PipelineConfig{
		Logger: zerolog.Nop(),
		CompanionDelegated: func(ctx context.Context, taskID string) (bool, error) {
			m, err := h.verify.CompanionDelegatedTasks(ctx, []string{taskID})
			return m[taskID], err
		},
		Provenance:       memory.NewProvenanceVerifier(root),
		RecordProvenance: h.verify.UpsertProvenance,
	})
	h.newWorker = func() *memory.IngestWorker {
		w := memory.NewIngestWorker(h.queue, h.arts, indexer, zerolog.Nop(), memory.IngestWorkerConfig{PollInterval: 50 * time.Millisecond})
		w.SetPipeline(pipeline)
		return w
	}
	return h
}

// body is a document long enough to pass the min_content gate, unique to the
// run (so no other test's chunk deduplicates it) and to its label.
func (h *gh482Harness) body(label string) string {
	return "# " + label + " " + h.project + "\n\nThis paragraph is the body of the " + label +
		" document. It carries enough words to pass the minimum content gate of the ingest pipeline.\n"
}

func (h *gh482Harness) task(t *testing.T, src persistence.TaskCreationSource, parent string) string {
	t.Helper()
	id := persistence.GenerateID("task-gh482")
	task := &persistence.Task{ID: id, ProjectID: h.project, CreationSource: src,
		Status: persistence.TaskStatusCompleted, Priority: 50, MaxAttempts: 1, Payload: []byte(`{}`)}
	if parent != "" {
		task.ParentTaskID = &parent
	}
	if src == persistence.TaskCreationSourceUser {
		op := "user:operator" // an operator's session
		task.CreatedByActor = &op
	}
	require.NoError(t, h.tasks.Create(context.Background(), task))
	return id
}

// taskWithActor creates a root task with an explicit principal ("" = none).
func (h *gh482Harness) taskWithActor(t *testing.T, src persistence.TaskCreationSource, principal string) string {
	t.Helper()
	id := persistence.GenerateID("task-gh482")
	task := &persistence.Task{ID: id, ProjectID: h.project, CreationSource: src,
		Status: persistence.TaskStatusCompleted, Priority: 50, MaxAttempts: 1, Payload: []byte(`{}`)}
	if principal != "" {
		task.CreatedByActor = &principal
	}
	require.NoError(t, h.tasks.Create(context.Background(), task))
	return id
}

// ingest stores an artifact of the task with these bytes and enqueues it the
// way the executor's hooks do: a document ingest carries the uploader's path.
func (h *gh482Harness) ingest(t *testing.T, taskID, producer, name, docPath, body string) string {
	t.Helper()
	ctx := context.Background()
	id := persistence.GenerateID("art-gh482")
	file := gh482Write(t, h.files, id+"/"+name, body)
	size := int64(len(body))
	task := taskID
	// A staged upload is an INPUT artifact (ingestInputArtifacts); a
	// workflow's file is an OUTPUT (ingestOutputArtifacts).
	class, origin := persistence.ArtifactClassOutput, persistence.ArtifactOriginTaskOutput
	if docPath != "" {
		class, origin = persistence.ArtifactClassInput, persistence.ArtifactOriginUpload
	}
	require.NoError(t, h.arts.Create(ctx, &persistence.Artifact{ID: id, ProjectID: h.project, TaskID: &task,
		Name: name, ArtifactClass: class, StoragePath: file, SizeBytes: &size, Origin: origin}))
	scope := h.scope
	item := &persistence.IngestQueueItem{ProjectID: h.project, SourceArtifactID: id, ProducerRole: producer,
		Priority: 50, ProposedConfidence: 0.5, RepoScope: &scope}
	if docPath != "" {
		p := docPath
		item.DocumentPath = &p
	}
	require.NoError(t, h.queue.Enqueue(ctx, item))
	h.enqueued = append(h.enqueued, id)
	return id
}

// drain runs the real worker loop until every enqueued item is done.
func (h *gh482Harness) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := h.newWorker()
	worker.Start(ctx)
	defer worker.Stop()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var pending int
		require.NoError(t, h.db.QueryRow(`SELECT COUNT(*) FROM project_ingest_queue
			WHERE project_id = $1 AND state IN ('queued','processing')`, h.project).Scan(&pending))
		if pending == 0 {
			break
		}
		require.True(t, time.Now().Before(deadline), "the ingest queue did not drain")
		time.Sleep(50 * time.Millisecond)
	}
	var failed int
	require.NoError(t, h.db.QueryRow(`SELECT COUNT(*) FROM project_ingest_queue
		WHERE project_id = $1 AND state <> 'done'`, h.project).Scan(&failed))
	require.Zero(t, failed, "an ingest item failed")
}

// state returns the distinct (status, validator) pairs of an artifact's chunks.
func (h *gh482Harness) state(t *testing.T, artifactID string) []string {
	t.Helper()
	rows, err := h.db.Query(`SELECT DISTINCT validation_status || '/' || COALESCE(validator_role, '')
		FROM project_memory_chunks WHERE project_id = $1 AND artifact_id = $2 ORDER BY 1`, h.project, artifactID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, out, "artifact %s produced no chunks", artifactID)
	return out
}

func TestIntegration_CompanionDelegatedChunksVerifiedOnlyByProvenance(t *testing.T) {
	h := newGH482Harness(t)
	merged := h.body("merged")
	const lld = memory.ProvenanceLLDDir + "/22-design.md"

	// An unpushed commit in the clone: on local main, not on origin/main.
	gh482Write(t, h.clone, memory.ProvenanceLLDDir+"/23-local.md", h.body("unpushed"))
	gh482Git(t, h.clone, "add", "-A")
	gh482Git(t, h.clone, "commit", "-q", "-m", "local only")

	companion := h.task(t, persistence.TaskCreationSourceCompanion, "")
	child := h.task(t, persistence.TaskCreationSourceDelegation, companion)
	operator := h.task(t, persistence.TaskCreationSourceUser, "")

	designOnMain := h.ingest(t, companion, "rag-ingester", "22-design.md", lld, merged)
	h.drain(t)
	// A forged version of the same design, from the same task (same-source
	// supersession) and from another companion task (document
	// supersession). Neither may retire the verified version.
	forged := h.ingest(t, companion, "rag-ingester", "22-design.md", lld, merged+"A forged ruling.\n")
	h.drain(t)
	other := h.task(t, persistence.TaskCreationSourceCompanion, "")
	forgedElsewhere := h.ingest(t, other, "rag-ingester", "22-design.md", lld, merged+"Another forged ruling.\n")
	unpushed := h.ingest(t, companion, "rag-ingester", "23-local.md", memory.ProvenanceLLDDir+"/23-local.md", h.body("unpushed"))
	offList := h.ingest(t, companion, "rag-ingester", "plan.md", "docs/plans/plan.md", h.body("plan"))
	review := h.ingest(t, companion, "reviewer", "review.md", "", h.body("companion review"))
	childReview := h.ingest(t, child, "reviewer", "review2.md", "", h.body("child review"))
	operatorReview := h.ingest(t, operator, "reviewer", "review3.md", "", h.body("operator review"))
	// Security finding 2026-10-22: a static key's task (the broker's) and a
	// task with no principal at all are not the operator's.
	staticKeyReview := h.ingest(t, h.taskWithActor(t, persistence.TaskCreationSourceUser, "system:static_key:0123456789abcdef"),
		"reviewer", "review4.md", "", h.body("static key review"))
	unattributedReview := h.ingest(t, h.taskWithActor(t, persistence.TaskCreationSourceUser, ""),
		"reviewer", "review5.md", "", h.body("unattributed review"))
	h.drain(t)

	// The merged design, its exact bytes: verified by provenance, with the
	// record that is the evidence, and still live after both forgeries.
	require.Equal(t, []string{"verified/" + memory.ValidatorRoleProvenance}, h.state(t, designOnMain))
	rec, err := h.verify.GetProvenance(context.Background(), h.project, designOnMain)
	require.NoError(t, err)
	require.NotNil(t, rec, "no provenance record for the verified design")
	require.Equal(t, memory.ProvenanceVerified, rec.State)
	require.Equal(t, gh482Git(t, h.clone, "rev-parse", "refs/remotes/origin/main"), rec.CommitSHA)
	require.Equal(t, gh482Git(t, h.clone, "rev-parse", "refs/remotes/origin/main:"+lld), rec.ContentBlobSHA)
	require.Equal(t, h.scope, rec.RepoScope)

	// #506 C6: any committed file can verify, so a plan committed on main
	// verifies like the design (it was "not on the list" before #506).
	require.Equal(t, []string{"verified/" + memory.ValidatorRoleProvenance}, h.state(t, offList), "a committed plan")

	// Everything else a companion delegated lands unverified.
	for name, c := range map[string]struct{ art, reason, state string }{
		"bytes not on main":      {forged, memory.ProvReasonBlobMismatch, "superseded/"}, // the newer forgery retired it, not the design
		"forged in another task": {forgedElsewhere, memory.ProvReasonBlobMismatch, "unverified/"},
		"unpushed commit":        {unpushed, memory.ProvReasonNotFound, "unverified/"},
		"companion review":       {review, "", "unverified/"},
		"child of a companion":   {childReview, "", "unverified/"},
		"static key task":        {staticKeyReview, "", "unverified/"},
		"unattributed task":      {unattributedReview, "", "unverified/"},
	} {
		require.Equal(t, []string{c.state}, h.state(t, c.art), name)
		rec, err := h.verify.GetProvenance(context.Background(), h.project, c.art)
		require.NoError(t, err, name)
		if c.reason == "" {
			require.Nil(t, rec, "%s: a non-document output has no provenance record", name)
			continue
		}
		require.NotNil(t, rec, name)
		require.Equal(t, memory.ProvenanceUnverified, rec.State, name)
		require.Equal(t, c.reason, rec.Reason, name)
	}

	// The control: the same reviewer output from an operator's task is still
	// role-of-record, so the test exercises the shortcut, not a broken one.
	require.Equal(t, []string{"verified/reviewer"}, h.state(t, operatorReview))
}
