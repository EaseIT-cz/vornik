package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// GitHub #368, design 22 "Every search path honours repo_scope and
// strict_scope", E4: the companion memory_correct claim search ignored the
// effective repo scope. The behavioural regression test is at the MCP
// tools/call level on the pgvector lane
// (test/integration/companion_recall_scope_test.go); these pin the Corrector's
// branch choice and its refusal.

// optsSearcher records the options of a scoped search.
type optsSearcher struct {
	stubSearcher
	lastOpts   *SearchOptions
	plainCalls int
}

func (s *optsSearcher) Search(ctx context.Context, p, q string, limit int) ([]SearchResult, error) {
	s.plainCalls++
	return s.stubSearcher.Search(ctx, p, q, limit)
}

func (s *optsSearcher) SearchWithOptions(_ context.Context, _, _ string, opts SearchOptions) ([]SearchResult, error) {
	o := opts
	s.lastOpts = &o
	return s.results, s.err
}

func TestCorrector_RefuteByClaimInScope_SearchesTheScope(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	expectClaimFacts(mock, "p", chunkFact{"chunk_a", "worker", "unverified", ""})
	expectClaimRefute(mock, "p", []string{"chunk_a"}, "chunk_a")
	s := &optsSearcher{stubSearcher: stubSearcher{results: []SearchResult{{ChunkID: "chunk_a", Content: "x", Score: 0.9}}}}
	c := NewCorrector(NewRepository(db), s)

	if _, err := c.RefuteByClaimInScope(context.Background(), "p", "claim", "github.com/example/repo-a", 5); err != nil {
		t.Fatal(err)
	}
	if s.lastOpts == nil {
		t.Fatal("a scoped claim search used the unscoped Search")
	}
	if s.lastOpts.RepoScope != "github.com/example/repo-a" || s.lastOpts.StrictScope || s.lastOpts.Limit != 5+claimRefuteOverFetch {
		t.Errorf("scoped search options = %+v, want the scope, lenient, limit 5 plus the #436 over-fetch", *s.lastOpts)
	}
	if s.plainCalls != 0 {
		t.Errorf("unscoped Search called %d time(s) alongside the scoped one", s.plainCalls)
	}
}

func TestCorrector_RefuteByClaim_EmptyScopeIsProjectWide(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	s := &optsSearcher{}
	c := NewCorrector(NewRepository(db), s)
	if _, err := c.RefuteByClaim(context.Background(), "p", "claim", 5); err != nil {
		t.Fatal(err)
	}
	if s.plainCalls != 1 || s.lastOpts != nil {
		t.Errorf("empty scope: plain=%d scoped=%v; want the project-wide Search", s.plainCalls, s.lastOpts)
	}
}

// A searcher that cannot apply a scope must refuse, never fall back to a
// project-wide search that could refute another repository's chunk.
func TestCorrector_RefuteByClaimInScope_RefusesWithoutScopedSearch(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	s := &stubSearcher{results: []SearchResult{{ChunkID: "chunk_b", Content: "x", Score: 0.9}}}
	c := NewCorrector(NewRepository(db), s)
	_, err := c.RefuteByClaimInScope(context.Background(), "p", "claim", "github.com/example/repo-a", 5)
	if !errors.Is(err, ErrScopedRefuteUnsupported) {
		t.Fatalf("err = %v, want ErrScopedRefuteUnsupported", err)
	}
	if s.lastLimit != 0 {
		t.Error("the unscoped Search ran")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a refute was written: %v", err)
	}
}
