package memory

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
)

// GitHub #76 (2026-10-05 audit): a Hermes memory forgotten and then added
// again never came back, because dedup_hash matched the refuted row's hash.
// Design 22, "Reinstating a refuted mirrored note" (round 2, strict D2).

func TestMarkRefutedByIDs_RecordsRoute(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	mock.ExpectExec(regexp.QuoteMeta("SET validation_status = 'refuted',")+
		`\s+`+regexp.QuoteMeta("refute_route = NULLIF($2, '')")).
		WithArgs("janka", RefuteRouteMirrorForget, "chunk_1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	n, err := repo.MarkRefutedByIDs(context.Background(), "janka", []string{"chunk_1"}, RefuteRouteMirrorForget)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReinstateMirroredByHash_OnlyRefutedMirroredRows(t *testing.T) {
	var nilR *Repository
	if id, err := nilR.ReinstateMirroredByHash(context.Background(), "p", "h", "companion:hermes"); id != "" || err != nil {
		t.Fatalf("nil repo: %q %v", id, err)
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	for _, args := range [][3]string{{"", "h", "r"}, {"p", "", "r"}, {"p", "h", ""}} {
		if id, err := r.ReinstateMirroredByHash(context.Background(), args[0], args[1], args[2]); id != "" || err != nil {
			t.Fatalf("empty arg %v: %q %v", args, id, err)
		}
	}
	stmt := regexp.QuoteMeta(`UPDATE project_memory_chunks
SET validation_status = 'unverified',
    refute_route = NULL,`) + `[\s\S]*` + regexp.QuoteMeta(`ELSE NOW() + (expires_at - created_at) END
WHERE project_id = $1
  AND content_hash = $2`) + `[\s\S]*` + regexp.QuoteMeta(`AND validation_status = 'refuted'
  AND refute_route = 'mirror_forget'
  AND producer_role = $3
  AND content LIKE '%⟦vm:%'`) + `[\s\S]*` + regexp.QuoteMeta(`RETURNING id`)
	mock.ExpectQuery(stmt).WithArgs("p", "h", "companion:hermes").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("chunk_1"))
	id, err := r.ReinstateMirroredByHash(context.Background(), "p", "h", "companion:hermes")
	if err != nil || id != "chunk_1" {
		t.Fatalf("reinstate: %q %v", id, err)
	}
	mock.ExpectQuery(stmt).WithArgs("p", "h2", "companion:hermes").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	id, err = r.ReinstateMirroredByHash(context.Background(), "p", "h2", "companion:hermes")
	if err != nil || id != "" {
		t.Fatalf("no row: %q %v", id, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

const reinstateNote = "[memory] My dentist is Dr Novak at the Vinohrady clinic in Prague. ⟦vm:0123456789abcdef⟧"

func reinstateRig(t *testing.T, hook func(ctx context.Context, projectID, hash, producerRole string) (string, error)) (*Pipeline, *[]CompanionIngestAuditEvent, func()) {
	t.Helper()
	p, _, _, _, cleanup := newPipelineTestRig(t)
	p.cfg.ChunkExists = func(context.Context, string, string) (bool, error) { return true, nil }
	p.cfg.ReinstateMirrored = hook
	var events []CompanionIngestAuditEvent
	p.cfg.RecordCompanionIngest = func(_ context.Context, ev CompanionIngestAuditEvent) error {
		events = append(events, ev)
		return nil
	}
	return p, &events, cleanup
}

func TestIngestCompanionNote_ReinstatesARefutedMirroredNote(t *testing.T) {
	type call struct{ project, hash, role string }
	var calls []call
	p, events, cleanup := reinstateRig(t, func(_ context.Context, projectID, hash, role string) (string, error) {
		calls = append(calls, call{projectID, hash, role})
		return "chunk_old", nil
	})
	defer cleanup()
	res, err := p.IngestCompanionNote(context.Background(), "proj", "hermes", "akey", "", reinstateNote, "", 0, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].project != "proj" || calls[0].hash != hashContent(reinstateNote) || calls[0].role != "companion:hermes" {
		t.Fatalf("hook calls = %+v", calls)
	}
	s := res.Stats
	if s.Admitted != 1 || s.Rejected != 0 || s.Reinstated != 1 || s.ReinstatedChunkID != "chunk_old" {
		t.Fatalf("stats = %+v", s)
	}
	for _, g := range s.GatesFailed {
		if g == string(GateDedupHash) {
			t.Fatalf("dedup_hash must not be reported on a reinstate: %v", s.GatesFailed)
		}
	}
	if len(*events) != 1 {
		t.Fatalf("audit events = %d", len(*events))
	}
	ev := (*events)[0]
	if ev.Decision != "admitted" || ev.GateFailed != "" || ev.ChunksAdmitted != 1 || ev.ReinstatedChunkID != "chunk_old" {
		t.Fatalf("audit = %+v", ev)
	}
	if got := testutil.ToFloat64(p.cfg.Metrics.ReinstatedTotal.WithLabelValues("proj")); got != 1 {
		t.Fatalf("reinstated metric = %v", got)
	}
	if got := testutil.ToFloat64(p.cfg.Metrics.PipelineRejectsTotal.WithLabelValues("proj", string(GateDedupHash))); got != 0 {
		t.Fatalf("reject metric = %v", got)
	}
}

func TestIngestCompanionNote_NoReinstateLeavesTheDedupReject(t *testing.T) {
	for name, hook := range map[string]func(context.Context, string, string, string) (string, error){
		"no row": func(context.Context, string, string, string) (string, error) { return "", nil },
		"error":  func(context.Context, string, string, string) (string, error) { return "", errors.New("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			p, events, cleanup := reinstateRig(t, hook)
			defer cleanup()
			res, err := p.IngestCompanionNote(context.Background(), "proj", "hermes", "akey", "", reinstateNote, "", 0, "", time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Stats.Admitted != 0 || res.Stats.Rejected != 1 || res.Stats.Reinstated != 0 {
				t.Fatalf("stats = %+v", res.Stats)
			}
			if strings.Join(res.Stats.GatesFailed, ",") != string(GateDedupHash) {
				t.Fatalf("gates = %v", res.Stats.GatesFailed)
			}
			if ev := (*events)[0]; ev.Decision != "rejected" || ev.GateFailed != string(GateDedupHash) || ev.ReinstatedChunkID != "" {
				t.Fatalf("audit = %+v", ev)
			}
		})
	}
}

func TestIngestArtifact_AgentPathNeverReinstates(t *testing.T) {
	called := 0
	p, _, cleanup := reinstateRig(t, func(context.Context, string, string, string) (string, error) {
		called++
		return "chunk_old", nil
	})
	defer cleanup()
	stats, err := p.IngestArtifact(context.Background(), "proj", "t", "a", "notes.md", "researcher", "exec", reinstateNote, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if called != 0 || stats.Rejected != 1 || stats.Admitted != 0 {
		t.Fatalf("agent ingest: hook called %d, stats %+v", called, stats)
	}
}

func TestCorrector_RefuteByIDs_RecordsTheRoute(t *testing.T) {
	for route, want := range map[string]string{
		RefuteRouteMirrorForget: RefuteRouteMirrorForget,
		"":                      RefuteRouteMemoryCorrect,
	} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		c := NewCorrector(NewRepository(db), nil)
		mock.ExpectExec(regexp.QuoteMeta("SET validation_status = 'refuted'")).
			WithArgs("p", want, "c1").WillReturnResult(sqlmock.NewResult(0, 1))
		if n, err := c.RefuteByIDs(context.Background(), "p", []string{"c1"}, route); err != nil || n != 1 {
			t.Fatalf("route %q: n=%d err=%v", route, n, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("route %q: %v", route, err)
		}
		_ = db.Close()
	}
}

// Review a6f0 (fix round 1) and a814 (fix round 2), D2: a note forgotten by
// the mirror and later refuted as wrong must stay refuted against an
// identical re-add. The strictest route wins, in ONE statement: a separate
// relabel UPDATE could miss a row a concurrent reinstate un-refuted between
// the two statements (READ COMMITTED re-checks the WHERE on the new row
// version; a transaction does not prevent that). Pins the statement shape;
// the race itself is exercised on Postgres by
// TestIntegration_Reinstate_RefuteRacingAReinstateIsNotLost.
func TestMarkRefutedByIDs_OneStatementStrictestRouteWins(t *testing.T) {
	stmt := regexp.QuoteMeta(`SET validation_status = 'refuted',
		    refute_route = NULLIF($2, '')
		WHERE project_id = $1
		  AND id IN ($3)
		  AND (validation_status NOT IN ('refuted', 'superseded')
		       OR (validation_status = 'refuted'
		           AND refute_route = 'mirror_forget'
		           AND $2 <> 'mirror_forget'))`)
	for _, route := range []string{RefuteRouteMemoryCorrect, RefuteRouteMirrorForget, ""} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		mock.ExpectExec(stmt).WithArgs("p", route, "c1").WillReturnResult(sqlmock.NewResult(0, 1))
		n, err := NewRepository(db).MarkRefutedByIDs(context.Background(), "p", []string{"c1"}, route)
		if err != nil || n != 1 {
			t.Fatalf("route %q: n=%d err=%v", route, n, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("route %q: %v (exactly one statement)", route, err)
		}
		_ = db.Close()
	}
}

// Review a6f0 minor 2: a non-allow gate before dedup_hash (secret_scan
// redacting) stays in the result's GatesFailed, but the reinstate's audit
// row is an admit and carries no gate_failed.
func TestIngestCompanionNote_ReinstateAfterRedactAuditsNoGateFailed(t *testing.T) {
	r, _, cleanup := newRepo(t)
	defer cleanup()
	var events []CompanionIngestAuditEvent
	p := NewPipeline(NewIndexer(Config{ChunkTokens: 512}, r, nil, zerolog.Nop()), PipelineConfig{
		Quarantine:              &fakeQuarantine{},
		SecretsDetector:         newSecretsDetector(t),
		ChunkExists:             func(context.Context, string, string) (bool, error) { return true, nil },
		ReinstateMirrored:       func(context.Context, string, string, string) (string, error) { return "chunk_old", nil },
		CreateCompanionArtifact: func(context.Context, string, string, string, int64) error { return nil },
		RecordCompanionIngest: func(_ context.Context, ev CompanionIngestAuditEvent) error {
			events = append(events, ev)
			return nil
		},
		Logger:  zerolog.Nop(),
		Metrics: freshMetrics(),
	})
	note := "[memory] The staging deploy key is sk-proj1234567890abcdefghijklmnopqrstuv and rotates every quarter for the whole team. ⟦vm:0123456789abcdef⟧"
	res, err := p.IngestCompanionNote(context.Background(), "proj", "hermes", "akey", "", note, "", 0, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Reinstated != 1 || strings.Join(res.Stats.GatesFailed, ",") != string(GateSecretScan) {
		t.Fatalf("stats = %+v (want reinstated, secret_scan kept, dedup_hash dropped)", res.Stats)
	}
	if len(events) != 1 || events[0].Decision != "admitted" || events[0].GateFailed != "" || events[0].ReinstatedChunkID != "chunk_old" {
		t.Fatalf("audit = %+v", events)
	}
}
