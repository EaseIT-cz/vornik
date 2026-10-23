package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunChunkGraphQuarantineReflagSuite pins ReflagQuarantinedChunks on both
// drivers: only quarantined chunks are selected, the error match is a literal
// case-sensitive substring, the report carries the denominator per project, a
// commit re-flags AND resets the counter, a non-quarantined chunk is never
// touched, and a re-run is a no-op. Every error string carries a per-run token
// so the all-projects calls touch nothing outside this suite on the shared
// Postgres integration database, and every per-chunk check reads that chunk's
// own row (state), never a page of the shared queue (#374).
//
// Knowledge-graph LLD §3.3b (2026-10-16, after GitHub #284). Found at:
// production — 1,760 chunks were quarantined by the invalid-UTF-8 mention bug
// and the only recovery verb, `vornikctl memory regraph`, re-flagged every
// edge-less chunk of a project (72,953 across the nine affected projects)
// while leaving graph_extraction_attempts at the bound, so each re-flagged
// chunk got one attempt before being quarantined again.
func RunChunkGraphQuarantineReflagSuite(t *testing.T, repo persistence.ChunkGraphExtractionRepository, seed SeedChunk, state ChunkGraphState) {
	t.Helper()
	fx := newQuarantineReflagFx(t, repo, seed)
	fx.state = state
	t.Run("dry_run_reports_the_denominator_and_writes_nothing", fx.dryRun)
	t.Run("match_is_a_literal_case_sensitive_substring", fx.literalMatch)
	t.Run("commit_reflags_and_resets_the_counter_of_matched_quarantined_chunks_only", fx.commit)
	t.Run("rerun_is_a_no_op", fx.rerun)
	t.Run("all_projects_selects_across_projects", fx.allProjects)
}

// ChunkGraphState reads one chunk's extraction bookkeeping by id: the
// needs_graph_extraction flag and graph_extraction_attempts.
type ChunkGraphState func(ctx context.Context, chunkID string) (needsExtraction bool, attempts int, err error)

type quarantineReflagFx struct {
	ctx                                    context.Context
	repo                                   persistence.ChunkGraphExtractionRepository
	state                                  ChunkGraphState
	tok, projA, projB, match               string
	aMatch, aOther, aWild, aPending, aDone string
	bMatch                                 string
}

func newQuarantineReflagFx(t *testing.T, repo persistence.ChunkGraphExtractionRepository, seed SeedChunk) *quarantineReflagFx {
	t.Helper()
	fx := &quarantineReflagFx{ctx: context.Background(), repo: repo, tok: uniqueID("tok"),
		projA: uniqueID("proj-a"), projB: uniqueID("proj-b")}
	fx.match = `insert mention: pq: invalid byte sequence for encoding "UTF8": 0x80 ` + fx.tok
	base := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	n := 0
	mk := func(project string) string {
		t.Helper()
		id := uniqueID("chunk")
		if err := seed(fx.ctx, id, project, "content of "+id, true, base.Add(time.Duration(n)*time.Minute)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		n++
		return id
	}
	fx.aMatch, fx.aOther, fx.aWild = mk(fx.projA), mk(fx.projA), mk(fx.projA)
	fx.quarantine(t, fx.aMatch, fx.match)
	fx.quarantine(t, fx.aOther, "insert mention for entity in chunk: not found "+fx.tok)
	fx.quarantine(t, fx.aWild, "a_b 100% "+fx.tok)
	// Not quarantined: a chunk that failed once under the bound (still
	// flagged, counter 1, matching error) and a chunk extracted cleanly (flag
	// clear, counter 0).
	fx.aPending = mk(fx.projA)
	if _, q, err := repo.RecordExtractionFailure(fx.ctx, fx.aPending, fx.match, 3); err != nil || q {
		t.Fatalf("pending failure: quarantined=%t err=%v", q, err)
	}
	fx.aDone = mk(fx.projA)
	if err := repo.MarkExtracted(fx.ctx, fx.aDone); err != nil {
		t.Fatalf("MarkExtracted: %v", err)
	}
	fx.bMatch = mk(fx.projB)
	fx.quarantine(t, fx.bMatch, fx.match)
	return fx
}

// quarantine fails a chunk once with a bound of 1: the flag clears, the
// counter is 1, the error is stored — the state the worker leaves.
func (fx *quarantineReflagFx) quarantine(t *testing.T, id, reason string) {
	t.Helper()
	if _, q, err := fx.repo.RecordExtractionFailure(fx.ctx, id, reason, 1); err != nil || !q {
		t.Fatalf("quarantine %s: quarantined=%t err=%v", id, q, err)
	}
}

func (fx *quarantineReflagFx) call(t *testing.T, project, contains string, countOnly bool) []persistence.QuarantineReflagCount {
	t.Helper()
	rows, err := fx.repo.ReflagQuarantinedChunks(fx.ctx, project, contains, countOnly)
	if err != nil {
		t.Fatalf("ReflagQuarantinedChunks(%q, %q, %t): %v", project, contains, countOnly, err)
	}
	return rows
}

func quarantineRow(rows []persistence.QuarantineReflagCount, project string) (persistence.QuarantineReflagCount, bool) {
	for _, r := range rows {
		if r.ProjectID == project {
			return r, true
		}
	}
	return persistence.QuarantineReflagCount{}, false
}

func wantQuarantineRow(t *testing.T, rows []persistence.QuarantineReflagCount, project string, q, m, re int) {
	t.Helper()
	got, ok := quarantineRow(rows, project)
	if !ok {
		t.Errorf("no row for project %s in %+v", project, rows)
		return
	}
	if got.Quarantined != q || got.Matched != m || got.ReFlagged != re {
		t.Errorf("project %s: quarantined/matched/reflagged = %d/%d/%d, want %d/%d/%d",
			project, got.Quarantined, got.Matched, got.ReFlagged, q, m, re)
	}
}

// fetched reports, for each of this suite's chunks, whether it is flagged
// for extraction — read row by row, so other suites' chunks cannot crowd it.
func (fx *quarantineReflagFx) fetched(t *testing.T) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	for _, id := range []string{fx.aMatch, fx.aOther, fx.aWild, fx.aPending, fx.aDone, fx.bMatch} {
		needs, _, err := fx.state(fx.ctx, id)
		if err != nil {
			t.Fatalf("state %s: %v", id, err)
		}
		got[id] = needs
	}
	return got
}

func (fx *quarantineReflagFx) attempts(t *testing.T, id string) int {
	t.Helper()
	_, n, err := fx.state(fx.ctx, id)
	if err != nil {
		t.Fatalf("state %s: %v", id, err)
	}
	return n
}

func (fx *quarantineReflagFx) dryRun(t *testing.T) {
	rows := fx.call(t, fx.projA, "invalid byte sequence", true)
	wantQuarantineRow(t, rows, fx.projA, 3, 1, 0)
	if _, ok := quarantineRow(rows, fx.projB); ok {
		t.Errorf("a project-scoped call reported another project: %+v", rows)
	}
	if fx.fetched(t)[fx.aMatch] {
		t.Error("a dry run re-flagged a chunk")
	}
	// No pattern: every quarantined chunk of the project matches.
	wantQuarantineRow(t, fx.call(t, fx.projA, "", true), fx.projA, 3, 3, 0)
}

func (fx *quarantineReflagFx) literalMatch(t *testing.T) {
	wantQuarantineRow(t, fx.call(t, fx.projA, "INVALID BYTE SEQUENCE", true), fx.projA, 3, 0, 0)
	// LIKE would read % and _ as wildcards and match "a_b 100% ".
	wantQuarantineRow(t, fx.call(t, fx.projA, "a%b", true), fx.projA, 3, 0, 0)
	wantQuarantineRow(t, fx.call(t, fx.projA, "a_b 100%", true), fx.projA, 3, 1, 0)
}

func (fx *quarantineReflagFx) commit(t *testing.T) {
	wantQuarantineRow(t, fx.call(t, fx.projA, "invalid byte sequence", false), fx.projA, 3, 1, 1)
	got := fx.fetched(t)
	if !got[fx.aMatch] {
		t.Error("the matched quarantined chunk was not re-flagged")
	}
	if got[fx.aOther] || got[fx.aWild] || got[fx.bMatch] {
		t.Errorf("an unmatched or out-of-project chunk was re-flagged: other=%t wild=%t projB=%t",
			got[fx.aOther], got[fx.aWild], got[fx.bMatch])
	}
	if got[fx.aDone] {
		t.Error("a cleanly extracted chunk (not quarantined) was re-flagged")
	}
	if n := fx.attempts(t, fx.aMatch); n != 0 {
		t.Errorf("re-flagged chunk attempts = %d, want 0", n)
	}
	if n := fx.attempts(t, fx.aPending); n != 1 {
		t.Errorf("the pending chunk's counter moved: %d, want 1", n)
	}
	// The counter was reset: the next failure is attempt 1 of a fresh budget
	// of 3, not attempt 2 of a counter left where the quarantine put it.
	if a, q, err := fx.repo.RecordExtractionFailure(fx.ctx, fx.aMatch, fx.match, 3); err != nil || a != 1 || q {
		t.Errorf("after re-flag a failure reads attempts=%d quarantined=%t err=%v, want 1/false", a, q, err)
	}
	// The pending chunk (flagged, counter 1, matching error) was not
	// quarantined, so it was not touched: its counter continues at 2.
	if a, _, err := fx.repo.RecordExtractionFailure(fx.ctx, fx.aPending, fx.match, 3); err != nil || a != 2 {
		t.Errorf("the pending chunk's counter was touched: attempts=%d err=%v, want 2", a, err)
	}
}

func (fx *quarantineReflagFx) rerun(t *testing.T) {
	wantQuarantineRow(t, fx.call(t, fx.projA, "invalid byte sequence", false), fx.projA, 2, 0, 0)
}

func (fx *quarantineReflagFx) allProjects(t *testing.T) {
	rows := fx.call(t, "", fx.tok, true)
	wantQuarantineRow(t, rows, fx.projA, 2, 2, 0)
	wantQuarantineRow(t, rows, fx.projB, 1, 1, 0)
	rows = fx.call(t, "", `invalid byte sequence for encoding "UTF8": 0x80 `+fx.tok, false)
	wantQuarantineRow(t, rows, fx.projB, 1, 1, 1)
	wantQuarantineRow(t, rows, fx.projA, 2, 0, 0)
	if !fx.fetched(t)[fx.bMatch] {
		t.Error("the all-projects commit did not re-flag the other project's chunk")
	}
}
