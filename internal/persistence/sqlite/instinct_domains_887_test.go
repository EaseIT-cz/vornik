package sqlite_test

// GitHub #887 (instinct tool-budget seam design, amendment 2026-11-23): SQLite
// refused every budget-domain instinct, because its instincts CHECK listed
// only the five original domains. Found at: unit, while writing the SQLite
// test of #886. These run the real driver's migration, where the CHECK lives.

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

const preFix887InstinctsDDL = `CREATE TABLE instincts (
    id               TEXT PRIMARY KEY,
    scope            TEXT NOT NULL DEFAULT 'project'
                     CHECK (scope IN ('project','global')),
    project_id       TEXT NOT NULL DEFAULT '',
    domain           TEXT NOT NULL
                     CHECK (domain IN ('recovery','cost','quality','retrieval','workflow')),
    trigger_key      TEXT NOT NULL,
    trigger_json     BLOB,
    action           TEXT NOT NULL,
    confidence       REAL NOT NULL DEFAULT 0,
    support_count    INTEGER NOT NULL DEFAULT 0,
    contradict_count INTEGER NOT NULL DEFAULT 0,
    source           TEXT NOT NULL DEFAULT 'observer'
                     CHECK (source IN ('observer','operator','architect-reject')),
    status           TEXT NOT NULL DEFAULT 'candidate'
                     CHECK (status IN ('candidate','active','promoted','retired')),
    distill_model    TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    last_seen_at     TEXT NOT NULL
)`

func TestInstinctsAcceptEveryDomain_887(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	insert := func(id, domain string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO instincts (id, scope, project_id, domain, trigger_key, trigger_json, action, confidence, status, source, created_at, updated_at, last_seen_at)
			VALUES (?, 'project', 'p', ?, ?, '{}', 'act', 0.5, 'candidate', 'observer', '2026-11-23T00:00:00.000000000Z', '2026-11-23T00:00:00.000000000Z', '2026-11-23T00:00:00.000000000Z')`, id, domain, "tk_"+id)
		return err
	}
	count := func() int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM instincts`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A fresh database accepts every domain.
	for _, d := range persistence.InstinctDomains() {
		if err := insert("fresh-"+d, d); err != nil {
			t.Errorf("fresh database refused domain %q: %v", d, err)
		}
	}

	// A database created before #887: the old CHECK, with a row and its evidence.
	for _, q := range []string{`DELETE FROM instinct_evidence`, `DROP TABLE instincts`, preFix887InstinctsDDL} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q[:20], err)
		}
	}
	if err := insert("old-cost", "cost"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO instinct_evidence (instinct_id, outcome_id, polarity, action, created_at) VALUES ('old-cost', 'o-1', 'support', 'act', '2026-11-23T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := insert("old-budget", persistence.InstinctDomainBudget); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("the pre-#887 table must refuse budget (the defect), got %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatalf("the rebuild kept %d rows, want 1", count())
	}
	var ev int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM instinct_evidence WHERE instinct_id = 'old-cost'`).Scan(&ev); err != nil || ev != 1 {
		t.Fatalf("evidence after the rebuild: %d %v, want 1", ev, err)
	}
	for _, d := range persistence.InstinctDomains() {
		if err := insert("after-"+d, d); err != nil {
			t.Errorf("migrated database refused domain %q: %v", d, err)
		}
	}
	// A second Migrate does not rebuild again (the marker is in place).
	var before string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='instincts'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, "domain: no CHECK (#887)") {
		t.Fatalf("the rebuilt table lacks its marker: %s", before)
	}
	n := count()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if count() != n {
		t.Fatalf("a second Migrate changed the row count: %d → %d", n, count())
	}
}
