package cli

// GitHub #482 (design 22, "Amendment 2026-10-21 (#482)", part 5): the
// operator backfill on a real SQLite store and a real clone. Never run
// against production: the verb is the operator's.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"vornik.io/vornik/internal/testutil/testenv"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryscope"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/spawn"
)

func revGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = testenv.Clean("GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.c", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.c")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func revWrite(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReverifyDelegated_PlanApplyRestore(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root, upstream := filepath.Join(base, "ws"), filepath.Join(base, "up")
	for _, d := range []string{root, upstream} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := spawn.RegisterWorkspaceRoot(root); err != nil {
		t.Fatal(err)
	}
	const project = "proj"
	design := "# Design 7\n\nThe merged rule, word for word.\n"
	revGit(t, upstream, "init", "-q", "-b", "main")
	revWrite(t, upstream, memory.ProvenanceLLDDir+"/07-x.md", design)
	revGit(t, upstream, "add", "-A")
	revGit(t, upstream, "commit", "-q", "-m", "init")
	revGit(t, root, "clone", "-q", upstream, project)
	scope := memoryscope.NormalizeRemote(upstream)

	db := sqlitetest.Memory(t)
	repo := sqlite.NewMemoryVerificationRepository(db.DB)
	tasks := sqlite.NewTaskRepository(db.DB)
	for id, src := range map[string]persistence.TaskCreationSource{
		"t-comp": persistence.TaskCreationSourceCompanion, "t-user": persistence.TaskCreationSourceUser,
	} {
		op := "user:operator" // an attributable operator principal
		if err := tasks.Create(ctx, &persistence.Task{ID: id, ProjectID: project, CreationSource: src,
			Status: persistence.TaskStatusCompleted, MaxAttempts: 1, Payload: []byte(`{}`), CreatedByActor: &op}); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(id, art, task, role, source, content string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
			(id, project_id, content, content_hash, created_at, artifact_id, task_id, producer_role, validator_role,
			 validation_status, source_name, repo_scope)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'verified', ?, ?)`,
			id, project, content, "h-"+id, persistence.SQLiteTime(time.Now()), art, task, role, role, source, scope); err != nil {
			t.Fatal(err)
		}
	}
	// a-path: a companion upload of the merged design under its path.
	// a-bare: a legacy upload of the same bytes named only "07-x.md".
	// a-stale: an older version of the design.
	// a-review: a companion-delegated review.
	// a-user: an operator task's review (left alone).
	// a-gone: an upload whose bytes are no longer stored.
	seed("c1", "a-path", "t-comp", "rag-ingester", memory.ProvenanceLLDDir+"/07-x.md", "The merged rule, word for word.")
	seed("c2", "a-bare", "t-comp", "rag-ingester", "07-x.md", "The merged rule, word for word.")
	seed("c3", "a-stale", "t-comp", "rag-ingester", memory.ProvenanceLLDDir+"/07-x.md", "An older rule.")
	seed("c4", "a-review", "t-comp", "reviewer", "review-1.md", "The merged rule, word for word.") // quotes the design
	seed("c5", "a-user", "t-user", "reviewer", "review-2.md", "GREEN.")
	seed("c6", "a-gone", "t-comp", "rag-ingester", memory.ProvenanceLLDDir+"/07-x.md", "gone")
	contents := map[string]string{"a-path": design, "a-bare": design, "a-stale": "# Design 7\n\nAn older rule.\n",
		"a-review": "GREEN, the merged rule, word for word.\n", "a-user": "GREEN.\n"}
	d := reverifyDeps{Repo: repo, Verifier: memory.NewProvenanceVerifier(root),
		Content: func(_ context.Context, id string) ([]byte, error) {
			if c, ok := contents[id]; ok {
				return []byte(c), nil
			}
			return nil, errors.New("not in the store")
		}}
	opt := reverifyOptions{Project: project, Sample: 5}

	plan, err := planReverify(ctx, d, opt)
	if err != nil {
		t.Fatal(err)
	}
	sampleReverify(ctx, d, opt, plan)
	got := map[string]string{}
	for _, r := range plan.Rows {
		got[r.Cand.ArtifactID] = r.Outcome + "/" + r.Reason
	}
	want := map[string]string{
		"a-path":   "verify/",
		"a-bare":   "verify/", // the blob proves which file the bare name is
		"a-stale":  "de-verify/" + memory.ProvReasonBlobMismatch,
		"a-review": "de-verify/" + memory.ProvReasonPathNotAllowed,
		"a-user":   "not-delegated/",
		"a-gone":   "de-verify/" + memory.ProvReasonContentUnavailable,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	var buf bytes.Buffer
	renderReverify(&buf, opt, plan)
	report := buf.String()
	for _, s := range []string{
		"examined 6 artifacts, 6 chunks",
		"not companion-delegated, left alone: 1 artifacts, 1 chunks",
		"verified by provenance:              2 artifacts, 2 chunks",
		"de-verified:                         3 artifacts, 3 chunks",
		"verified: 2 of 2 sampled chunks are text of their file",
		// The review quotes the design: its text is on main, so the sample
		// flags it for a look, without calling it a false de-verification.
		"de-verified: 1 of 3 sampled chunks have their text still on main",
	} {
		if !strings.Contains(report, s) {
			t.Errorf("report lacks %q:\n%s", s, report)
		}
	}

	// Nothing was written by the dry run.
	var verified int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_memory_chunks WHERE validation_status='verified'`).Scan(&verified)
	if verified != 6 {
		t.Fatalf("the dry run changed chunks: %d verified, want 6", verified)
	}

	ev, err := applyReverify(ctx, d, opt, plan, "operator:test", "issue 482 backfill")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Examined != 6 || ev.Unchanged != 1 || ev.Verified != 2 || ev.Deverified != 3 {
		t.Fatalf("event: %+v", ev)
	}
	state := func(id string) string {
		var st, v string
		_ = db.QueryRowContext(ctx, `SELECT validation_status, COALESCE(validator_role,'') FROM project_memory_chunks WHERE id=?`, id).Scan(&st, &v)
		return st + "/" + v
	}
	for id, w := range map[string]string{
		"c1": "verified/" + memory.ValidatorRoleProvenance, "c2": "verified/" + memory.ValidatorRoleProvenance,
		"c3": "unverified/", "c4": "unverified/", "c5": "verified/reviewer", "c6": "unverified/",
	} {
		if got := state(id); got != w {
			t.Errorf("after apply %s = %q, want %q", id, got, w)
		}
	}
	if p, _ := repo.GetProvenance(ctx, project, "a-bare"); p == nil || p.State != memory.ProvenanceVerified || p.DocumentPath != memory.ProvenanceLLDDir+"/07-x.md" {
		t.Fatalf("the bare-name verification left no record of its path: %+v", p)
	}

	n, err := repo.CountRestorableSelfCertified(ctx, ev.ID)
	if err != nil || n != 5 {
		t.Fatalf("restorable self-certified = %d (%v), want 5", n, err)
	}
	rs, err := repo.RestoreVerificationEvent(ctx, ev.ID, "operator:test", "rollback drill")
	if err != nil || rs.Verified != 5 {
		t.Fatalf("restore: %+v %v", rs, err)
	}
	for id, w := range map[string]string{"c1": "verified/rag-ingester", "c4": "verified/reviewer", "c6": "verified/rag-ingester"} {
		if got := state(id); got != w {
			t.Errorf("after restore %s = %q, want %q", id, got, w)
		}
	}
}

func TestCandidatePaths(t *testing.T) {
	files := map[string]string{memory.ProvenanceLLDDir + "/a.md": "1", "CLAUDE.md": "2"}
	// A path is tried first, then the eligible files with its base name (a
	// path that is not eligible can still be the bytes of one that is).
	if got := candidatePaths("docs/x/a.md", files); len(got) != 2 || got[0] != "docs/x/a.md" || got[1] != memory.ProvenanceLLDDir+"/a.md" {
		t.Fatalf("a path, then its base name: %v", got)
	}
	if got := candidatePaths(memory.ProvenanceLLDDir+"/a.md", files); len(got) != 1 {
		t.Fatalf("an eligible path is not tried twice: %v", got)
	}
	if got := candidatePaths("a.md", files); len(got) != 1 || got[0] != memory.ProvenanceLLDDir+"/a.md" {
		t.Fatalf("a bare name maps by base name: %v", got)
	}
	if got := candidatePaths("review.md", files); len(got) != 0 {
		t.Fatalf("no file of that name: %v", got)
	}
}

// GitHub #506 C8 (design 22, "Amendment 2026-10-22 (#506)"): the backfill
// never removes trust it cannot disprove. A companion-delegated document in a
// repository with no clone is kept verified as provenance:unavailable; before
// #506 it was de-verified, so --apply on a multi-repository companion project
// de-verified every repository but one. Found at: production (the
// 2026-10-22 dry run on the reference host: 25,704 chunks to de-verify).
func TestReverifyDelegated506_UnexaminableIsKeptVerified(t *testing.T) {
	ctx := context.Background()
	const project = "proj506"
	db := sqlitetest.Memory(t)
	repo := sqlite.NewMemoryVerificationRepository(db.DB)
	tasks := sqlite.NewTaskRepository(db.DB)
	op := "user:operator"
	if err := tasks.Create(ctx, &persistence.Task{ID: "t-comp", ProjectID: project, CreationSource: persistence.TaskCreationSourceCompanion,
		Status: persistence.TaskStatusCompleted, MaxAttempts: 1, Payload: []byte(`{}`), CreatedByActor: &op}); err != nil {
		t.Fatal(err)
	}
	const scope = "github.com/acme/elsewhere"
	if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
		(id, project_id, content, content_hash, created_at, artifact_id, task_id, producer_role, validator_role,
		 validation_status, source_name, repo_scope)
		VALUES ('c1', ?, 'body', 'h-c1', ?, 'a-elsewhere', 't-comp', 'rag-ingester', 'rag-ingester', 'verified', ?, ?)`,
		project, persistence.SQLiteTime(time.Now()), memory.ProvenanceLLDDir+"/01-x.md", scope); err != nil {
		t.Fatal(err)
	}
	v := memory.NewProvenanceVerifier(t.TempDir())
	v.CloneFor = func(context.Context, string, string) (string, string) { return "", memory.ProvReasonCloneFailed }
	d := reverifyDeps{Repo: repo, Verifier: v,
		Content: func(context.Context, string) ([]byte, error) { return []byte("body"), nil }}
	opt := reverifyOptions{Project: project, Sample: 1}
	plan, err := planReverify(ctx, d, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rows) != 1 || plan.Rows[0].Outcome != reverifyUnavailable {
		t.Fatalf("rows = %+v, want one unavailable", plan.Rows)
	}
	var buf bytes.Buffer
	renderReverify(&buf, opt, plan)
	if !strings.Contains(buf.String(), "kept, provenance unavailable:        1 artifacts, 1 chunks") {
		t.Fatalf("report:\n%s", buf.String())
	}
	if _, err := applyReverify(ctx, d, opt, plan, "operator:test", "issue 506"); err != nil {
		t.Fatal(err)
	}
	var st, role string
	_ = db.QueryRowContext(ctx, `SELECT validation_status, COALESCE(validator_role,'') FROM project_memory_chunks WHERE id='c1'`).Scan(&st, &role)
	if st != "verified" || role != memory.ValidatorRoleProvenanceUnavailable {
		t.Fatalf("after apply: %s/%s, want verified/%s (never de-verified)", st, role, memory.ValidatorRoleProvenanceUnavailable)
	}
}

// Code review fbe6 F1: the backfill's unavailable relabel touches only the
// role-of-record stamp the plan saw; a provenance stamp on another chunk of
// the artifact is never overwritten.
func TestReverifyDelegated506_UnavailableNeverOverwritesProvenance(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	repo := sqlite.NewMemoryVerificationRepository(db.DB)
	plan := &reverifyPlan{Rows: []reverifyRow{{Cand: persistence.VerificationCandidate{ArtifactID: "a-mixed",
		ProducerRole: "rag-ingester", RepoScope: "github.com/acme/x", Chunks: 2}, Outcome: reverifyUnavailable,
		Result: memory.ProvenanceResult{State: memory.ProvenanceUnverified, Reason: memory.ProvReasonCloneFailed,
			Path: "doc.md", CheckedAt: time.Now()}}}}
	for id, role := range map[string]string{"c-ror": "rag-ingester", "c-prov": memory.ValidatorRoleProvenance} {
		if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
			(id, project_id, content, content_hash, created_at, artifact_id, producer_role, validator_role, validation_status, source_name)
			VALUES (?, 'p', 'x', ?, ?, 'a-mixed', 'rag-ingester', ?, 'verified', 'doc.md')`,
			id, "h-"+id, persistence.SQLiteTime(time.Now()), role); err != nil {
			t.Fatal(err)
		}
	}
	d := reverifyDeps{Repo: repo}
	if _, err := applyReverify(ctx, d, reverifyOptions{Project: "p"}, plan, "operator:test", "fbe6"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"c-ror": memory.ValidatorRoleProvenanceUnavailable, "c-prov": memory.ValidatorRoleProvenance} {
		var role string
		_ = db.QueryRowContext(ctx, `SELECT COALESCE(validator_role,'') FROM project_memory_chunks WHERE id=?`, id).Scan(&role)
		if role != want {
			t.Errorf("%s: role %q, want %q", id, role, want)
		}
	}
}
