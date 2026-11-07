//go:build integration
// +build integration

package integration_test

// GitHub #639, design 22 "Amendment 2026-11-04 (#639)": a project mirrors
// repository paths into its memory.
//
// Found at: review (architect design §5.1: a changed brief or public page
// was withdrawn by the sweep but its new version never arrived; observed in
// production on 2026-11-04, where only a temporary companion key could
// bring docs/public into vornik-architect verified). Regression test at:
// integration, the real sweep pass (MirrorAll, the gated pipeline, the
// indexer and the provenance store) on the pgvector database with a real
// git clone. Before the change nothing mirrored, so every assertion that a
// chunk exists fails.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryscope"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/spawn"
)

type mirror639 struct {
	db       *sql.DB
	project  string
	scope    string
	upstream string
	clone    string
	sweeper  *memory.ProvenanceSweeper
}

func newMirror639(t *testing.T) *mirror639 {
	t.Helper()
	db := connectDB(t)
	t.Cleanup(func() { _ = db.Close() })
	m := &mirror639{db: db, project: fmt.Sprintf("itest-mirror639-%d", time.Now().UnixNano())}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM project_memory_chunks WHERE project_id = $1`,
			`DELETE FROM project_memory_quarantine WHERE project_id = $1`,
			`DELETE FROM memory_chunk_provenance WHERE project_id = $1`,
			`DELETE FROM artifacts WHERE project_id = $1`,
		} {
			_, _ = db.Exec(q, m.project)
		}
	})
	base := t.TempDir()
	root := filepath.Join(base, "workspaces")
	m.upstream = filepath.Join(base, "upstream")
	require.NoError(t, os.MkdirAll(m.upstream, 0o755))
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, spawn.RegisterWorkspaceRoot(root))
	gh482Git(t, m.upstream, "init", "-q", "-b", "main")
	gh482Write(t, m.upstream, "docs/public/a.md", m.body("page A, version one"))
	gh482Write(t, m.upstream, "docs/public/hidden.md", "---\ndisclosure: never\n---\n"+m.body("hidden page"))
	gh482Write(t, m.upstream, "docs/other/b.md", m.body("outside the prefix"))
	gh482Git(t, m.upstream, "add", "-A")
	gh482Git(t, m.upstream, "commit", "-q", "-m", "init")
	gh482Git(t, root, "clone", "-q", m.upstream, m.project)
	m.clone = filepath.Join(root, m.project)
	m.scope = memoryscope.NormalizeRemote(m.upstream)

	verify := postgres.NewMemoryVerificationRepository(db)
	quarantine := postgres.NewMemoryQuarantineRepository(db)
	repo := memory.NewRepository(db)
	cfg := memory.DefaultConfig()
	cfg.Enabled = true
	cfg.EmbeddingEndpoint = ""
	indexer := memory.NewIndexer(cfg, repo, nil, zerolog.Nop())
	verifier := memory.NewProvenanceVerifier(root)
	pipeline := memory.NewPipeline(indexer, memory.PipelineConfig{
		Logger: zerolog.Nop(),
		// The mirror never asks: a ProvenanceOnly ingest is delegated.
		CompanionDelegated: func(context.Context, string) (bool, error) { return false, nil },
		Provenance:         verifier,
		RecordProvenance:   verify.UpsertProvenance,
		Quarantine:         quarantine,
	})
	supersede := func(ctx context.Context, projectID, scope, path string) (int, error) {
		return repo.SupersedeDocument(ctx, projectID, scope, path, "")
	}
	m.sweeper = &memory.ProvenanceSweeper{
		Store: verify, Verifier: verifier, Logger: zerolog.Nop(), Supersede: supersede,
		Mirror: memory.MirrorHooks{
			Projects: func() []memory.MirrorProject {
				return []memory.MirrorProject{{ProjectID: m.project, RepoScope: m.scope, Paths: []string{"docs/public/"}}}
			},
			Provenance:  verify.GetProvenance,
			Quarantined: quarantine.ExistsForArtifact,
			Ingest: memory.MirrorIngester{Artifacts: postgres.NewArtifactRepository(db), Pipeline: pipeline,
				Supersede: supersede}.Ingest,
		},
	}
	return m
}

func (m *mirror639) body(label string) string {
	return "# " + label + " " + m.project + "\n\nThis paragraph is the body of the " + label +
		" document. It carries enough words to pass the minimum content gate of the ingest pipeline.\n"
}

// live counts a path's chunks that are not superseded or refuted, and those
// of them stamped verified by provenance with the run's scope.
func (m *mirror639) live(t *testing.T, path string) (all, verified int) {
	t.Helper()
	require.NoError(t, m.db.QueryRow(`SELECT COUNT(*),
		COUNT(*) FILTER (WHERE validation_status = 'verified' AND validator_role = $3 AND repo_scope = $4)
		FROM project_memory_chunks
		WHERE project_id = $1 AND source_name = $2 AND validation_status NOT IN ('superseded','refuted')`,
		m.project, path, memory.ValidatorRoleProvenance, m.scope).Scan(&all, &verified))
	return all, verified
}

func (m *mirror639) artifacts(t *testing.T, path string) int {
	t.Helper()
	var n int
	require.NoError(t, m.db.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE project_id = $1 AND name = $2`, m.project, path).Scan(&n))
	return n
}

func TestIntegration_RepoMirror_639(t *testing.T) {
	m := newMirror639(t)
	ctx := context.Background()

	m.sweeper.SweepAll(ctx)
	all, verified := m.live(t, "docs/public/a.md")
	require.Positive(t, all, "the mirrored page has no chunks")
	require.Equal(t, all, verified, "every chunk of the mirrored page must be provenance-verified in the run's scope")
	hidden, _ := m.live(t, "docs/public/hidden.md")
	require.Zero(t, hidden, "a page marked disclosure: never was mirrored")
	outside, _ := m.live(t, "docs/other/b.md")
	require.Zero(t, outside, "a file outside the prefix was mirrored")

	// A second pass ingests nothing new.
	m.sweeper.SweepAll(ctx)
	require.Equal(t, 1, m.artifacts(t, "docs/public/a.md"), "an unchanged version was ingested again")

	// A changed page: the new version is mirrored and verified, and the old
	// one superseded.
	gh482Write(t, m.upstream, "docs/public/a.md", m.body("page A, version two"))
	gh482Git(t, m.upstream, "commit", "-q", "-am", "edit a")
	gh482Git(t, m.clone, "fetch", "-q", "origin")
	m.sweeper.SweepAll(ctx)
	require.Equal(t, 2, m.artifacts(t, "docs/public/a.md"))
	all, verified = m.live(t, "docs/public/a.md")
	require.Positive(t, verified)
	require.Equal(t, all, verified, "the old version's chunks are still live beside the new one")
	var text string
	require.NoError(t, m.db.QueryRow(`SELECT string_agg(content, ' ') FROM project_memory_chunks
		WHERE project_id = $1 AND source_name = 'docs/public/a.md' AND validation_status NOT IN ('superseded','refuted')`,
		m.project).Scan(&text))
	require.Contains(t, text, "version two")
	require.NotContains(t, text, "version one")
}
