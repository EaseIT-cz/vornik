//go:build integration

package memory_test

// GitHub #76 (2026-10-05 audit): a Hermes memory that was forgotten (its
// mirrored chunk refuted) and then added again was REJECTED dedup_hash
// against the refuted row and never came back. Design 22, "Reinstating a
// refuted mirrored note" (round 2, strict D2): only a tokenised note the
// mirror itself forgot (refute_route = 'mirror_forget') is reinstated, and
// only by a companion of the same client kind through IngestCompanionNote.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

func newReinstateStack(t *testing.T, db *sql.DB) (*memory.Pipeline, *memory.Searcher, *memory.Repository) {
	t.Helper()
	repo := memory.NewRepository(db)
	cfg := memory.DefaultConfig()
	cfg.Enabled = true
	cfg.EmbeddingEndpoint = "" // keyword arm only: deterministic
	indexer := memory.NewIndexer(cfg, repo, nil, zerolog.Nop())
	pipeline := memory.NewPipeline(indexer, memory.PipelineConfig{
		Logger: zerolog.Nop(),
		CreateCompanionArtifact: func(_ context.Context, projectID, artifactID, sourceName string, _ int64) error {
			seedArtifact(t, db, projectID, artifactID, sourceName)
			return nil
		},
		// Wired as container_scheduler.go wires them.
		ChunkExists:       repo.ChunkExistsByHash,
		ReinstateMirrored: repo.ReinstateMirroredByHash,
	})
	searcher := memory.NewSearcher(cfg, repo, nil)
	// container_scheduler.go wires an epoch source wherever it builds the
	// pipeline, and only the epoch-aware search excludes refuted rows. No
	// active epochs = companion notes (epoch_id NULL) are read as legacy.
	searcher.SetEpochSource(func(context.Context, string) ([]string, error) { return []string{}, nil })
	return pipeline, searcher, repo
}

func deposit(t *testing.T, p *memory.Pipeline, projectID, clientKind, note string) memory.IngestStats {
	t.Helper()
	res, err := p.IngestCompanionNote(context.Background(), projectID, clientKind, "akey-"+clientKind,
		"companion:"+clientKind+":note", note, "", 0, "", time.Time{})
	if err != nil {
		t.Fatalf("IngestCompanionNote: %v", err)
	}
	return res.Stats
}

func onlyChunk(t *testing.T, db *sql.DB, projectID string) (id, status string, route sql.NullString) {
	t.Helper()
	rows, err := db.Query(`SELECT id, validation_status, refute_route FROM project_memory_chunks WHERE project_id = $1`, projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
		if err := rows.Scan(&id, &status, &route); err != nil {
			t.Fatal(err)
		}
	}
	if n != 1 {
		t.Fatalf("project %s holds %d chunks, want 1", projectID, n)
	}
	return id, status, route
}

// recalled runs the companion recall path (memoryCompanionAdapter.Recall →
// RecallWithContext) over the epoch-aware search, which excludes refuted and
// expired rows.
func recalled(t *testing.T, s *memory.Searcher, projectID, query, chunkID string) bool {
	t.Helper()
	res, err := s.RecallWithContext(context.Background(), projectID, query, memory.SearchOptions{Limit: 20},
		memoryfirewall.RequestContext{Role: "companion:hermes", OperatorID: "akey-hermes", Purpose: memoryfirewall.PurposeOperational})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range res {
		if r.ChunkID == chunkID {
			return true
		}
	}
	return false
}

// (a) Forgotten by the mirror, added again: recallable, with its TTL restarted.
func TestIntegration_Reinstate_MirrorForgetIsReinstated(t *testing.T) {
	db := openIngestRecallDB(t)
	p, s, repo := newReinstateStack(t, db)
	project := persistence.GenerateID("itest-reinstate-a")
	t.Cleanup(func() { cleanupProjects(t, db, project) })

	note, token := mirrorNote("user", "My dentist is Dr Novak at the Vinohrady clinic in Prague, booked every spring.")
	if st := deposit(t, p, project, "hermes", note); st.Admitted != 1 {
		t.Fatalf("first deposit: %+v", st)
	}
	id, _, _ := onlyChunk(t, db, project)
	if n, err := repo.MarkRefutedByIDs(context.Background(), project, []string{id}, memory.RefuteRouteMirrorForget); err != nil || n != 1 {
		t.Fatalf("refute: %d %v", n, err)
	}
	// The forget happened late in a 30-day life, and the re-add comes after expiry.
	if _, err := db.Exec(`UPDATE project_memory_chunks SET created_at = NOW() - INTERVAL '31 days',
		expires_at = NOW() - INTERVAL '1 day' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if recalled(t, s, project, token, id) {
		t.Fatal("test setup: a refuted, expired note is recalled")
	}

	st := deposit(t, p, project, "hermes", note)
	if st.Admitted != 1 || st.Reinstated != 1 || st.ReinstatedChunkID != id {
		t.Fatalf("re-add: %+v", st)
	}
	_, status, route := onlyChunk(t, db, project)
	if status != "unverified" || route.Valid {
		t.Fatalf("after reinstate: status=%s route=%v", status, route)
	}
	var left time.Duration
	var secs float64
	if err := db.QueryRow(`SELECT EXTRACT(EPOCH FROM expires_at - NOW()) FROM project_memory_chunks WHERE id = $1`, id).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	left = time.Duration(secs) * time.Second
	if left < 29*24*time.Hour || left > 30*24*time.Hour+time.Hour {
		t.Fatalf("expires_at restarted to %v from now, want about the 30-day span", left)
	}
	if !recalled(t, s, project, token, id) {
		t.Fatal("a reinstated note must be recallable")
	}
}

// (b) Any other route, NULL included, or another client kind: stays refuted.
func TestIntegration_Reinstate_OtherRoutesAndWritersStayRefuted(t *testing.T) {
	db := openIngestRecallDB(t)
	p, s, repo := newReinstateStack(t, db)
	cases := []struct {
		name, route, reAddClient string
	}{
		{"forget command", memory.RefuteRouteForgetCommand, "hermes"},
		{"memory_correct", memory.RefuteRouteMemoryCorrect, "hermes"},
		{"chat forget", memory.RefuteRouteChatForget, "hermes"},
		{"NULL route (refuted before the column)", "", "hermes"},
		{"mirror forget, re-sent by another client kind", memory.RefuteRouteMirrorForget, "claude-code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project := persistence.GenerateID("itest-reinstate-b")
			t.Cleanup(func() { cleanupProjects(t, db, project) })
			note, token := mirrorNote("memory", "Prefers Czech for invoices and English for everything else, always.")
			deposit(t, p, project, "hermes", note)
			id, _, _ := onlyChunk(t, db, project)
			if !recalled(t, s, project, token, id) {
				t.Fatal("test setup: the live note is not recalled")
			}
			if _, err := repo.MarkRefutedByIDs(context.Background(), project, []string{id}, tc.route); err != nil {
				t.Fatal(err)
			}
			st := deposit(t, p, project, tc.reAddClient, note)
			if st.Admitted != 0 || st.Reinstated != 0 || st.Rejected != 1 {
				t.Fatalf("re-add: %+v", st)
			}
			if _, status, _ := onlyChunk(t, db, project); status != "refuted" {
				t.Fatalf("status = %s, want refuted", status)
			}
			if recalled(t, s, project, token, id) {
				t.Fatal("a note refuted by another route came back")
			}
		})
	}
}

// (c) No token, or superseded: never reinstated.
func TestIntegration_Reinstate_UntokenisedAndSupersededStay(t *testing.T) {
	db := openIngestRecallDB(t)
	p, _, repo := newReinstateStack(t, db)

	t.Run("untokenised note refuted as mirror_forget", func(t *testing.T) {
		project := persistence.GenerateID("itest-reinstate-c1")
		t.Cleanup(func() { cleanupProjects(t, db, project) })
		note := "[user] The office printer is on the third floor beside the kitchen, model Brother."
		deposit(t, p, project, "hermes", note)
		id, _, _ := onlyChunk(t, db, project)
		if _, err := repo.MarkRefutedByIDs(context.Background(), project, []string{id}, memory.RefuteRouteMirrorForget); err != nil {
			t.Fatal(err)
		}
		if st := deposit(t, p, project, "hermes", note); st.Reinstated != 0 || st.Admitted != 0 {
			t.Fatalf("re-add: %+v", st)
		}
		if _, status, _ := onlyChunk(t, db, project); status != "refuted" {
			t.Fatalf("status = %s", status)
		}
	})

	t.Run("superseded mirrored note", func(t *testing.T) {
		project := persistence.GenerateID("itest-reinstate-c2")
		t.Cleanup(func() { cleanupProjects(t, db, project) })
		note, _ := mirrorNote("memory", "The family car is a blue Skoda Octavia estate bought in 2021.")
		deposit(t, p, project, "hermes", note)
		id, _, _ := onlyChunk(t, db, project)
		if _, err := db.Exec(`UPDATE project_memory_chunks SET validation_status = 'superseded',
			refute_route = 'mirror_forget' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if st := deposit(t, p, project, "hermes", note); st.Reinstated != 0 || st.Admitted != 0 {
			t.Fatalf("re-add: %+v", st)
		}
		if _, status, _ := onlyChunk(t, db, project); status != "superseded" {
			t.Fatalf("status = %s", status)
		}
	})
}

// Review a6f0 (fix round 1), D2: forgotten by the mirror, then refuted as
// wrong by another route, then re-added identically: stays refuted. And the
// reverse order never downgrades a stricter route to mirror_forget.
func TestIntegration_Reinstate_StrictestRouteWins(t *testing.T) {
	db := openIngestRecallDB(t)
	p, s, repo := newReinstateStack(t, db)
	ctx := context.Background()

	t.Run("mirror forget then memory_correct", func(t *testing.T) {
		project := persistence.GenerateID("itest-reinstate-d1")
		t.Cleanup(func() { cleanupProjects(t, db, project) })
		note, token := mirrorNote("user", "My accountant is Ing. Dvorak in Brno, who files the VAT returns each quarter.")
		deposit(t, p, project, "hermes", note)
		id, _, _ := onlyChunk(t, db, project)
		if n, err := repo.MarkRefutedByIDs(ctx, project, []string{id}, memory.RefuteRouteMirrorForget); err != nil || n != 1 {
			t.Fatalf("mirror forget: %d %v", n, err)
		}
		if n, err := repo.MarkRefutedByIDs(ctx, project, []string{id}, memory.RefuteRouteMemoryCorrect); err != nil || n != 1 {
			t.Fatalf("memory_correct on a refuted row: n=%d err=%v (the relabel to a stricter route is counted)", n, err)
		}
		if _, _, route := onlyChunk(t, db, project); route.String != memory.RefuteRouteMemoryCorrect {
			t.Fatalf("route = %v, want memory_correct", route)
		}
		if st := deposit(t, p, project, "hermes", note); st.Reinstated != 0 || st.Admitted != 0 {
			t.Fatalf("re-add: %+v", st)
		}
		if recalled(t, s, project, token, id) {
			t.Fatal("a fact refuted as wrong came back")
		}
	})

	t.Run("mirror forget then an empty route records NULL", func(t *testing.T) {
		project := persistence.GenerateID("itest-reinstate-d3")
		t.Cleanup(func() { cleanupProjects(t, db, project) })
		note, _ := mirrorNote("memory", "The spare house key is with the neighbour at number twelve, Mrs Horakova.")
		deposit(t, p, project, "hermes", note)
		id, _, _ := onlyChunk(t, db, project)
		if _, err := repo.MarkRefutedByIDs(ctx, project, []string{id}, memory.RefuteRouteMirrorForget); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.MarkRefutedByIDs(ctx, project, []string{id}, ""); err != nil {
			t.Fatal(err)
		}
		if _, status, route := onlyChunk(t, db, project); status != "refuted" || route.Valid {
			t.Fatalf("status=%s route=%v, want refuted with NULL route", status, route)
		}
		if st := deposit(t, p, project, "hermes", note); st.Reinstated != 0 {
			t.Fatalf("re-add: %+v", st)
		}
	})

	t.Run("memory_correct then mirror forget", func(t *testing.T) {
		project := persistence.GenerateID("itest-reinstate-d2")
		t.Cleanup(func() { cleanupProjects(t, db, project) })
		note, _ := mirrorNote("user", "The gym membership renews on the first of March at the Karlin branch.")
		deposit(t, p, project, "hermes", note)
		id, _, _ := onlyChunk(t, db, project)
		if _, err := repo.MarkRefutedByIDs(ctx, project, []string{id}, memory.RefuteRouteMemoryCorrect); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.MarkRefutedByIDs(ctx, project, []string{id}, memory.RefuteRouteMirrorForget); err != nil {
			t.Fatal(err)
		}
		if _, _, route := onlyChunk(t, db, project); route.String != memory.RefuteRouteMemoryCorrect {
			t.Fatalf("route = %v, want memory_correct kept", route)
		}
		if st := deposit(t, p, project, "hermes", note); st.Reinstated != 0 {
			t.Fatalf("re-add: %+v", st)
		}
	})
}

// Review a814 (fix round 2): a refute as wrong that races a reinstate of the
// same mirror-forgotten row must not be lost. The interleaving is made
// deterministic: transaction A holds the reinstate uncommitted, the refute
// runs and blocks on A's row lock (observed in pg_stat_activity), then A
// commits. With the refute split into two statements, the first saw the old
// refuted row and did nothing, and the second re-checked its WHERE against
// the reinstated row and matched nothing: the row ended valid. One statement
// re-checks both branches against the new row version and refutes it.
func TestIntegration_Reinstate_RefuteRacingAReinstateIsNotLost(t *testing.T) {
	db := openIngestRecallDB(t)
	p, _, repo := newReinstateStack(t, db)
	ctx := context.Background()
	project := persistence.GenerateID("itest-reinstate-race")
	t.Cleanup(func() { cleanupProjects(t, db, project) })
	note, _ := mirrorNote("user", "My landlord is Mr Prochazka, rent is due on the fifth of every month.")
	deposit(t, p, project, "hermes", note)
	id, _, _ := onlyChunk(t, db, project)
	if _, err := repo.MarkRefutedByIDs(ctx, project, []string{id}, memory.RefuteRouteMirrorForget); err != nil {
		t.Fatal(err)
	}

	txA, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback() }()
	if _, err := txA.ExecContext(ctx, `UPDATE project_memory_chunks
SET validation_status = 'unverified', refute_route = NULL
WHERE id = $1 AND validation_status = 'refuted' AND refute_route = 'mirror_forget'`, id); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := repo.MarkRefutedByIDs(ctx, project, []string{id}, memory.RefuteRouteMemoryCorrect)
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
WHERE wait_event_type = 'Lock' AND query LIKE '%UPDATE project_memory_chunks%' AND query LIKE '%refute_route%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the refute finished without waiting for the reinstate's row lock (err=%v)", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the refute never blocked on the reinstate's row lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := txA.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, status, route := onlyChunk(t, db, project); status != "refuted" || route.String != memory.RefuteRouteMemoryCorrect {
		t.Fatalf("status=%s route=%v: the refute as wrong was lost to the racing reinstate", status, route)
	}
}
