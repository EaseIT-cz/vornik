package memory

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #368, design 22 "Every search path honours repo_scope and
// strict_scope": a widen round grows the candidate pool and never relaxes the
// caller's strict_scope. Before the fix both widen helpers set
// StrictScope=false, which handed a strict caller the NULL-scoped chunks it
// had excluded. The behavioural regression tests are on the pgvector lane
// (scope_every_path_integration_test.go); these pin the option builders.
func TestWidenOptions_KeepStrictScope(t *testing.T) {
	base := SearchOptions{Limit: 10, RepoScope: "github.com/example/repo-a", StrictScope: true}
	for round := 2; round <= 4; round++ {
		if got := widenOptions(base, round); !got.StrictScope || got.RepoScope != base.RepoScope {
			t.Errorf("sufficiency round %d: %+v dropped the caller's scope", round, got)
		}
	}
	for round := 1; round <= 3; round++ {
		if got := widenRoutingOptions(base, round); !got.StrictScope || got.RepoScope != base.RepoScope {
			t.Errorf("routing widen round %d: %+v dropped the caller's scope", round, got)
		}
	}
	lenient := SearchOptions{Limit: 10, RepoScope: "github.com/example/repo-a"}
	if widenOptions(lenient, 2).StrictScope || widenRoutingOptions(lenient, 1).StrictScope {
		t.Error("a widen round made a lenient caller strict")
	}
}

// The memory store is PostgreSQL-only: on the SQLite schema every search
// statement fails, so a recall there returns an error and no rows, scoped or
// not (measured 2026-10-16, GitHub #368). This pins that the SQLite lane can
// never return an out-of-scope row: it returns none at all.
func TestSQLite_SearchReturnsNoRowsScopedOrNot(t *testing.T) {
	db := sqlitetest.Memory(t)
	if _, err := db.Exec(`INSERT INTO project_memory_chunks (id, project_id, source_name, content, repo_scope, created_at)
		VALUES ('c-b', 'p', 'b.md', 'zorbquill relay in repository bravo', 'github.com/example/repo-b', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := NewRepository(db.DB)
	ctx := context.Background()
	var none time.Time
	for _, tc := range []struct {
		name   string
		epochs bool
		scope  string
		strict bool
	}{
		{"no epoch, scoped", false, "github.com/example/repo-a", false},
		{"no epoch, strict", false, "github.com/example/repo-a", true},
		{"no epoch, unscoped", false, "", false},
		{"epoch, scoped", true, "github.com/example/repo-a", false},
		{"epoch, strict", true, "github.com/example/repo-a", true},
	} {
		res, err := r.HybridSearchWithEpochs(ctx, "p", nil, "zorbquill", 10, nil, tc.epochs, none, none, tc.scope, tc.strict)
		if err == nil {
			t.Errorf("%s: no error on SQLite; the measurement in design 22 no longer holds", tc.name)
		}
		if len(res) != 0 {
			t.Errorf("%s: %d rows returned on SQLite", tc.name, len(res))
		}
	}
}
