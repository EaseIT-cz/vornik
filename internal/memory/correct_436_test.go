package memory

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

// GitHub #436 (design 22, "Amendment 2026-10-21 (#436): a claim refute never
// refutes an operator correction"). Found at: review (the 2026-10-17
// security audit). The behavioural regression test is at the companion MCP
// tools/call level on the pgvector lane
// (test/integration/companion_memory_correct_436_test.go); these pin the
// Corrector's plan: exclusion, ordering, over-fetch, the skipped report and
// the report-what-was-written rule.

const claimFactsSQL = "SELECT id, COALESCE(producer_role, ''), COALESCE(validation_status, ''), COALESCE(artifact_id, '')"
const claimRefuteSQL = "RETURNING id"

type chunkFact struct{ id, role, status, artifact string }

func expectClaimFacts(mock sqlmock.Sqlmock, project string, facts ...chunkFact) {
	args := []driver.Value{project}
	rows := sqlmock.NewRows([]string{"id", "producer_role", "validation_status", "artifact_id", "validator_role"})
	for _, f := range facts {
		args = append(args, f.id)
		rows.AddRow(f.id, f.role, f.status, f.artifact, "")
	}
	mock.ExpectQuery(regexp.QuoteMeta(claimFactsSQL)).WithArgs(args...).WillReturnRows(rows)
}

func expectClaimRefute(mock sqlmock.Sqlmock, project string, ids []string, flipped ...string) {
	args := []driver.Value{project, RefuteRouteMemoryCorrect, ProducerRoleOperatorCorrection, companionCorrectionArtifactLike, ValidatorRoleProvenance}
	for _, id := range ids {
		args = append(args, id)
	}
	rows := sqlmock.NewRows([]string{"id"})
	for _, id := range flipped {
		rows.AddRow(id)
	}
	mock.ExpectQuery(regexp.QuoteMeta(claimRefuteSQL)).WithArgs(args...).WillReturnRows(rows)
}

func skipReasons(res *ClaimRefuteResult) map[string]string {
	out := map[string]string{}
	for _, s := range res.Skipped {
		out[s.ID] = s.Reason
	}
	return out
}

func refutedIDs(res *ClaimRefuteResult) []string {
	out := make([]string, 0, len(res.Refuted))
	for _, r := range res.Refuted {
		out = append(out, r.ID)
	}
	return out
}

// The issue's scenario: the correction ranks first; it is never refuted, and
// the stale chunk behind it is.
func TestCorrector_ClaimRefute_NeverRefutesOperatorCorrection(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	expectClaimFacts(mock, "p",
		chunkFact{"corr", ProducerRoleOperatorCorrection, "verified", ""},
		chunkFact{"stale", "worker", "unverified", ""})
	expectClaimRefute(mock, "p", []string{"stale"}, "stale")

	c := NewCorrector(NewRepository(db), &stubSearcher{results: []SearchResult{
		{ChunkID: "corr", Content: "X listens on 9090", Score: 0.9, ValidationStatus: "verified"},
		{ChunkID: "stale", Content: "X listens on 9090 (old)", Score: 0.4, ValidationStatus: "unverified"},
	}})
	res, err := c.RefuteClaim(context.Background(), "p", "X listens on 9090", "", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := refutedIDs(res); len(got) != 1 || got[0] != "stale" {
		t.Fatalf("refuted = %v, want [stale]", got)
	}
	if r := skipReasons(res)["corr"]; r != SkipReasonOperatorCorrection {
		t.Errorf("correction skip reason = %q, want %q", r, SkipReasonOperatorCorrection)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// When only corrections clear the floor, nothing is written at all.
func TestCorrector_ClaimRefute_OnlyCorrectionsWritesNothing(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	expectClaimFacts(mock, "p", chunkFact{"corr", ProducerRoleOperatorCorrection, "verified", ""})
	// No refute expectation: an UPDATE would fail the mock.
	c := NewCorrector(NewRepository(db), &stubSearcher{results: []SearchResult{
		{ChunkID: "corr", Content: "c", Score: 0.9},
	}})
	res, err := c.RefuteClaim(context.Background(), "p", "claim", "", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Refuted) != 0 {
		t.Fatalf("refuted = %v, want none", refutedIDs(res))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Unverified chunks are refuted before verified ones; the verified one is
// reported over_cap; the below-floor one is reported too.
func TestCorrector_ClaimRefute_PrefersUnverifiedAndReportsEverySkip(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	expectClaimFacts(mock, "p",
		chunkFact{"ver", "rag-ingester", "verified", ""},
		chunkFact{"unv", "worker", "unverified", ""})
	expectClaimRefute(mock, "p", []string{"unv"}, "unv")

	c := NewCorrector(NewRepository(db), &stubSearcher{results: []SearchResult{
		{ChunkID: "ver", Content: "v", Score: 0.8},
		{ChunkID: "unv", Content: "u", Score: 0.6},
		{ChunkID: "low", Content: "l", Score: 0.01},
	}})
	res, err := c.RefuteClaim(context.Background(), "p", "claim", "", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := refutedIDs(res); len(got) != 1 || got[0] != "unv" {
		t.Fatalf("refuted = %v, want [unv]", got)
	}
	reasons := skipReasons(res)
	if reasons["ver"] != SkipReasonOverCap || reasons["low"] != SkipReasonBelowFloor {
		t.Errorf("skip reasons = %v, want ver=over_cap low=below_floor", reasons)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The report is what the UPDATE wrote: a chosen id it did not flip is
// not_flipped, never refuted.
func TestCorrector_ClaimRefute_ReportsOnlyFlippedRows(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	expectClaimFacts(mock, "p",
		chunkFact{"a", "worker", "unverified", ""},
		chunkFact{"b", "worker", "unverified", ""})
	expectClaimRefute(mock, "p", []string{"a", "b"}, "a")

	c := NewCorrector(NewRepository(db), &stubSearcher{results: []SearchResult{
		{ChunkID: "a", Content: "a", Score: 0.9},
		{ChunkID: "b", Content: "b", Score: 0.8},
	}})
	res, err := c.RefuteClaim(context.Background(), "p", "claim", "", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := refutedIDs(res); len(got) != 1 || got[0] != "a" {
		t.Fatalf("refuted = %v, want [a]", got)
	}
	if r := skipReasons(res)["b"]; r != SkipReasonNotFlipped {
		t.Errorf("b skip reason = %q, want not_flipped", r)
	}
}

// dry_run plans and reports, and writes nothing.
func TestCorrector_ClaimRefute_DryRunWritesNothing(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	expectClaimFacts(mock, "p", chunkFact{"a", "worker", "unverified", ""})
	c := NewCorrector(NewRepository(db), &stubSearcher{results: []SearchResult{
		{ChunkID: "a", Content: "a", Score: 0.9},
	}})
	res, err := c.RefuteClaim(context.Background(), "p", "claim", "", 3, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || len(res.Refuted) != 1 || res.Refuted[0].ID != "a" {
		t.Fatalf("dry run = %+v, want would-refute [a]", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The search over-fetches so a protected correction cannot crowd out the
// stale chunk.
func TestCorrector_ClaimRefute_OverFetches(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	s := &stubSearcher{}
	c := NewCorrector(NewRepository(db), s)
	if _, err := c.RefuteClaim(context.Background(), "p", "claim", "", 1, false); err != nil {
		t.Fatal(err)
	}
	if s.lastLimit != 1+claimRefuteOverFetch {
		t.Errorf("search limit = %d, want %d", s.lastLimit, 1+claimRefuteOverFetch)
	}
}

// The pipeline's only producer_role writer refuses the reserved literal, so
// no ingest path can mint a chunk the claim path protects.
func TestPatchPolicyByArtifact_RefusesReservedProducerRole(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	r := NewRepository(db)
	exp := time.Now()
	err := r.PatchPolicyByArtifact(context.Background(), "p", "art", "decision", 0.9, ProducerRoleOperatorCorrection, "exec", &exp, "")
	if !errors.Is(err, ErrReservedProducerRole) {
		t.Fatalf("err = %v, want ErrReservedProducerRole", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL ran for a reserved role: %v", err)
	}
}

// The pipeline refuses the reserved role before any write, so an ingest of
// a workflow step configured with role operator_correction never lands a
// chunk at all.
func TestPipeline_IngestRefusesReservedProducerRole(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	p := NewPipeline(NewIndexer(Config{ChunkTokens: 512}, NewRepository(db), nil, zerolog.Nop()), PipelineConfig{Logger: zerolog.Nop()})
	_, err := p.IngestArtifactWithOptions(context.Background(), "p", "task", "art", "out.md",
		ProducerRoleOperatorCorrection, "exec", "some workflow output", 20, "", IngestArtifactOptions{})
	if !errors.Is(err, ErrReservedProducerRole) {
		t.Fatalf("err = %v, want ErrReservedProducerRole", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL ran for a reserved role: %v", err)
	}
}

// GitHub #436 part 2: a companion memory_correct correction is protected by
// its daemon-minted artifact prefix together with a companion role; a
// remember() note (prefix "companion_") and a workflow chunk carrying a
// lookalike artifact id without a companion role are not.
func TestIsProtectedCorrection(t *testing.T) {
	cases := []struct {
		name string
		f    claimChunkFacts
		want bool
	}{
		{"operator correction", claimChunkFacts{ProducerRole: ProducerRoleOperatorCorrection}, true},
		{"companion correction", claimChunkFacts{ProducerRole: "companion:claude-code", ArtifactID: "companioncorrection_20261021_ab"}, true},
		{"companion note", claimChunkFacts{ProducerRole: "companion:claude-code", ArtifactID: "companion_20261021_ab"}, false},
		{"agent role on a lookalike artifact", claimChunkFacts{ProducerRole: "worker", ArtifactID: "companioncorrection_20261021_ab"}, false},
		{"unscoped chunk", claimChunkFacts{}, false},
	}
	for _, c := range cases {
		if got := isProtectedCorrection(c.f); got != c.want {
			t.Errorf("%s: isProtectedCorrection = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCorrector_ClaimRefute_NeverRefutesCompanionCorrection(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	expectClaimFacts(mock, "p",
		chunkFact{"corr", "companion:claude-code", "unverified", "companioncorrection_20261021_ab"},
		chunkFact{"stale", "worker", "unverified", "art_1"})
	expectClaimRefute(mock, "p", []string{"stale"}, "stale")
	c := NewCorrector(NewRepository(db), &stubSearcher{results: []SearchResult{
		{ChunkID: "corr", Content: "c", Score: 0.9},
		{ChunkID: "stale", Content: "s", Score: 0.4},
	}})
	res, err := c.RefuteClaim(context.Background(), "p", "claim", "", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := refutedIDs(res); len(got) != 1 || got[0] != "stale" {
		t.Fatalf("refuted = %v, want [stale]", got)
	}
	if r := skipReasons(res)["corr"]; r != SkipReasonOperatorCorrection {
		t.Errorf("companion correction skip reason = %q", r)
	}
}
