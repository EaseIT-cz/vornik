package memory

// audit 2026-10-12 (GDPR increment 6, GitHub #343): Art 18 requests were recorded
// and nothing withheld a restricted subject's memory from use. These tests pin the
// screen at the two seams every recall and listing path ends in — and pin that the
// recall screen runs BEFORE the LLM reranker, because sending restricted text to a
// model is processing even if the result is then dropped.
//
// see LLD § https://docs.vornik.io §4.16.1

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

type fakeRestrictionScreen struct {
	restricted map[string]bool
	err        error
	calls      int
}

func (f *fakeRestrictionScreen) RestrictedChunks(_ context.Context, ids []string) (map[string]bool, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]bool{}
	for _, id := range ids {
		if f.restricted[id] {
			out[id] = true
		}
	}
	return out, nil
}

// recordingReranker records which chunks the model was shown.
type recordingReranker struct{ saw []string }

func (r *recordingReranker) Rerank(_ context.Context, _ string, results []SearchResult) ([]SearchResult, error) {
	for _, res := range results {
		r.saw = append(r.saw, res.ChunkID)
	}
	return results, nil
}

func TestSearch_RestrictedChunkNeverReachesTheReranker(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetLogger(zerolog.Nop())
	rr := &recordingReranker{}
	s.SetReranker(rr)
	screen := &fakeRestrictionScreen{restricted: map[string]bool{"c2": true}}
	s.SetRestrictionScreen(screen)

	mock.ExpectQuery("ts_rank").WillReturnRows(makeRR([]string{"c1", "c2", "c3"}, []float64{0.9, 0.8, 0.7}))

	got, err := s.SearchWithOptions(context.Background(), "p", "q", SearchOptions{Limit: 5, Rerank: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range rr.saw {
		if id == "c2" {
			t.Fatal("the restricted chunk was sent to the reranker model — processing under Art 18(2)")
		}
	}
	if len(rr.saw) == 0 {
		t.Fatal("reranker never ran; the test proves nothing")
	}
	for _, res := range got {
		if res.ChunkID == "c2" {
			t.Fatal("the restricted chunk was returned")
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want the 2 unrestricted", len(got))
	}
}

// "A lift takes effect at once" rests on there being no cache between the screen
// and the restriction table (change review round 1, F2): one Searcher must
// reflect a restriction appearing and disappearing between consecutive recalls.
func TestSearch_RestrictionIsReadLiveOnEveryRecall(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetLogger(zerolog.Nop())
	screen := &fakeRestrictionScreen{restricted: map[string]bool{}}
	s.SetRestrictionScreen(screen)

	recall := func() []SearchResult {
		t.Helper()
		mock.ExpectQuery("ts_rank").WillReturnRows(makeRR([]string{"c1"}, []float64{0.9}))
		got, err := s.Search(context.Background(), "p", "q", 5)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if len(recall()) != 1 {
		t.Fatal("unrestricted chunk missing")
	}
	screen.restricted["c1"] = true // restriction committed
	if len(recall()) != 0 {
		t.Fatal("a restriction committed between recalls was not honoured by the next one")
	}
	delete(screen.restricted, "c1") // lifted
	if len(recall()) != 1 {
		t.Fatal("a lift was not honoured by the next recall — something cached the answer")
	}
	if screen.calls != 3 {
		t.Fatalf("screen consulted %d times for 3 recalls", screen.calls)
	}
}

// The screen fails CLOSED: a lookup error withholds the whole candidate set and
// surfaces as an error, never as unfiltered results.
func TestSearch_RestrictionScreenErrorFailsClosed(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetLogger(zerolog.Nop())
	s.SetRestrictionScreen(&fakeRestrictionScreen{err: errors.New("db down")})

	mock.ExpectQuery("ts_rank").WillReturnRows(makeRR([]string{"c1"}, []float64{0.9}))

	got, err := s.Search(context.Background(), "p", "q", 5)
	if err == nil {
		t.Fatal("a failed restriction lookup must fail the recall, not return unfiltered results")
	}
	if len(got) != 0 {
		t.Fatalf("results leaked past a failed screen: %+v", got)
	}
}

// With NO firewall wired (applyFirewall is then a passthrough) the listing seam
// must still withhold a restricted chunk: the restriction cannot depend on the
// firewall being configured or in enforce mode.
func TestRecentWithContext_ExcludesRestrictedWithoutAFirewall(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetLogger(zerolog.Nop())
	s.SetRestrictionScreen(&fakeRestrictionScreen{restricted: map[string]bool{"blocked": true}})

	recentAuditExpectRecentQuery(mock, "clean", "blocked")

	rows, err := s.RecentWithContext(context.Background(), "p", 10, "", false, false, memoryfirewall.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ChunkID != "clean" {
		t.Fatalf("got %+v, want only the unrestricted chunk", rows)
	}
}

func TestRecentWithContext_ScreenErrorWithholdsEverything(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetLogger(zerolog.Nop())
	s.SetRestrictionScreen(&fakeRestrictionScreen{err: errors.New("db down")})

	recentAuditExpectRecentQuery(mock, "clean", "blocked")

	rows, err := s.RecentWithContext(context.Background(), "p", 10, "", false, false, memoryfirewall.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a failed screen must withhold the listing, got %+v", rows)
	}
}

// The production constructor wires the screen, so the control cannot be lost to a
// forgotten setter.
func TestNew_WiresTheRestrictionScreen(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	m, err := New(Config{}, db, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if m.Searcher.restriction == nil {
		t.Fatal("memory.New must install the restriction screen")
	}
}

func TestRepository_RestrictedChunks(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta(persistence.RestrictedChunkIDsSQL("$1,$2"))).
		WithArgs("c1", "c2").
		WillReturnRows(sqlmock.NewRows([]string{"row_id"}).AddRow("c2"))

	got, err := r.RestrictedChunks(context.Background(), []string{"c1", "c2"})
	if err != nil {
		t.Fatal(err)
	}
	if !got["c2"] || got["c1"] {
		t.Fatalf("got %v", got)
	}
	if got, err := r.RestrictedChunks(context.Background(), nil); err != nil || len(got) != 0 {
		t.Fatalf("empty input: %v %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Consolidation sends chunk text to a model with no firewall seam, so the
// restriction is in its SQL.
func TestListChunkContents_ExcludesRestrictedChunks(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta(persistence.ChunkNotRestrictedSQL("id"))).
		WillReturnRows(sqlmock.NewRows([]string{"content"}))
	if _, err := r.ListChunkContents(context.Background(), "p", 5); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ListChunkContents does not carry the restriction predicate: %v", err)
	}
}
