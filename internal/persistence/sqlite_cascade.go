package persistence

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// FKAction is what a parent delete does to a child row on Postgres.
type FKAction int

const (
	// FKCascade deletes the child row with its parent (ON DELETE CASCADE).
	FKCascade FKAction = iota + 1
	// FKSetNull clears the child's reference column (ON DELETE SET NULL).
	FKSetNull
)

func (a FKAction) String() string {
	switch a {
	case FKCascade:
		return "CASCADE"
	case FKSetNull:
		return "SET NULL"
	}
	return fmt.Sprintf("FKAction(%d)", int(a))
}

// FKEdge is one foreign key from Child.Column to Parent(id).
type FKEdge struct {
	Child, Column, Parent string
	Action                FKAction
	// PostgresOnly marks an edge whose child column does not exist on SQLite
	// (the table, or just the column); the SQLite executor skips it. The
	// drift test fails if the column ever appears on SQLite, so the flag
	// cannot go stale silently.
	PostgresOnly bool
}

// SQLiteCascadeGraph is the Postgres ON DELETE CASCADE / SET NULL graph below
// every parent table SQLite code deletes from (tasks, executions, artifacts;
// and, for the project wipe only, knowledge_entities and project_memory_chunks
// — GitHub #365).
// SQLite opens with foreign_keys(OFF) (sqlite.go, Connect), so none of these
// fire there; sqliteDeleteCascading applies them explicitly. This list is the
// ONE place the graph is written down for SQLite. It is checked against the
// SQLite schema (sqlite package, unit lane) and the Postgres catalog
// (postgres package, integration lane): a new child table on either driver
// with no entry here fails one of them.
//
// Order within a parent matters only for readability: every statement selects
// its rows through the still-present parent rows.
//
// GitHub #361 (2026-10-13).
// see LLD § https://docs.vornik.io §6
var SQLiteCascadeGraph = []FKEdge{
	// tasks
	{Child: "tasks", Column: "parent_task_id", Parent: "tasks", Action: FKSetNull},
	{Child: "executions", Column: "task_id", Parent: "tasks", Action: FKCascade},
	{Child: "artifacts", Column: "task_id", Parent: "tasks", Action: FKSetNull},
	{Child: "webhook_events", Column: "task_id", Parent: "tasks", Action: FKSetNull},
	{Child: "autonomy_evaluations", Column: "task_id", Parent: "tasks", Action: FKSetNull},
	{Child: "trading_orders", Column: "task_id", Parent: "tasks", Action: FKSetNull},
	{Child: "task_judge_verdicts", Column: "task_id", Parent: "tasks", Action: FKCascade},
	{Child: "task_post_mortems", Column: "task_id", Parent: "tasks", Action: FKCascade},
	{Child: "task_messages", Column: "task_id", Parent: "tasks", Action: FKCascade},
	{Child: "task_scratchpad", Column: "task_id", Parent: "tasks", Action: FKCascade, PostgresOnly: true},
	{Child: "telegram_task_threads", Column: "task_id", Parent: "tasks", Action: FKCascade},
	{Child: "task_credentials", Column: "task_id", Parent: "tasks", Action: FKCascade},
	// Declared by the baseline schema (deployments/postgres/schema/
	// 001_initial.sql), not a migration; SQLite's chunk table has no task_id.
	{Child: "project_memory_chunks", Column: "task_id", Parent: "tasks", Action: FKSetNull, PostgresOnly: true},

	// executions
	{Child: "artifacts", Column: "execution_id", Parent: "executions", Action: FKCascade},
	{Child: "execution_narration", Column: "execution_id", Parent: "executions", Action: FKCascade},
	{Child: "execution_quality_scores", Column: "execution_id", Parent: "executions", Action: FKCascade},
	{Child: "llm_exchanges", Column: "execution_id", Parent: "executions", Action: FKCascade},
	{Child: "task_messages", Column: "execution_id", Parent: "executions", Action: FKSetNull},
	{Child: "task_scratchpad", Column: "last_execution_id", Parent: "executions", Action: FKSetNull, PostgresOnly: true},
	{Child: "corpus_epochs", Column: "ingest_execution_id", Parent: "executions", Action: FKSetNull},
	{Child: "project_ingest_queue", Column: "ingest_execution_id", Parent: "executions", Action: FKSetNull},
	{Child: "project_memory_quarantine", Column: "ingest_execution_id", Parent: "executions", Action: FKSetNull, PostgresOnly: true},

	// artifacts
	{Child: "project_ingest_queue", Column: "source_artifact_id", Parent: "artifacts", Action: FKCascade},
	{Child: "project_memory_quarantine", Column: "source_artifact_id", Parent: "artifacts", Action: FKSetNull, PostgresOnly: true},
	// SQLite's chunk table carries artifact_id (not task_id), nullable.
	{Child: "project_memory_chunks", Column: "artifact_id", Parent: "artifacts", Action: FKSetNull},

	// knowledge_entities and project_memory_chunks: parents the project wipe
	// deletes from (GitHub #365, 2026-10-13; archival LLD "2026-10-13 — the
	// wipe on SQLite" C3). No retention step deletes either.
	{Child: "knowledge_edges", Column: "from_entity", Parent: "knowledge_entities", Action: FKCascade},
	{Child: "knowledge_edges", Column: "to_entity", Parent: "knowledge_entities", Action: FKCascade},
	{Child: "entity_mentions", Column: "entity_id", Parent: "knowledge_entities", Action: FKCascade},
	{Child: "entity_mentions", Column: "chunk_id", Parent: "project_memory_chunks", Action: FKCascade},
	{Child: "memory_embed_queue", Column: "chunk_id", Parent: "project_memory_chunks", Action: FKCascade, PostgresOnly: true},
	{Child: "memory_embed_dlq", Column: "chunk_id", Parent: "project_memory_chunks", Action: FKCascade, PostgresOnly: true},
	{Child: "project_memory_quarantine", Column: "released_chunk_id", Parent: "project_memory_chunks", Action: FKSetNull, PostgresOnly: true},
}

// SQLiteCascadeParent reports whether deleting from table has children in
// SQLiteCascadeGraph — i.e. whether a SQLite delete from it must go through
// sqliteDeleteCascading.
func SQLiteCascadeParent(table string) bool {
	for _, e := range SQLiteCascadeGraph {
		if e.Parent == table && !e.PostgresOnly {
			return true
		}
	}
	return false
}

// sqlIdent bounds every identifier the executor interpolates. The graph is a
// package constant; this is the defensive check pruneOlderThan also makes.
var sqlIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// sqliteDeleteCascading deletes the rows of parent matching where (bound with
// args) and, first, applies every SQLiteCascadeGraph edge below them: a
// SET NULL edge clears the child's column, a CASCADE edge deletes the child
// rows after recursing into the child's own edges. It returns the parent
// delete's RowsAffected — the count a plain DELETE reported.
//
// Unexported so every caller goes through SQLiteDeleteCascadingTx: a failing
// child statement must leave the parent and every child in place, and only a
// transaction gives that (review round 2, N1).
//
// where is embedded exactly once in every statement, so args bind each
// statement in either placeholder style (? or $N). where is SQL text, not a
// value: callers pass package constants only. Contract: where must not read a
// column the graph nulls on the parent itself (it is evaluated again after
// the SET NULL statements); the check below is a substring match, so it may
// refuse a predicate whose identifier merely contains the column name — it
// errs toward refusing.
//
// GitHub #361 (2026-10-13): SQLite runs with foreign_keys(OFF), so a plain
// parent delete orphaned every child.
func sqliteDeleteCascading(ctx context.Context, q DBTX, parent, where string, args ...any) (int64, error) {
	if !sqlIdent.MatchString(parent) {
		return 0, fmt.Errorf("sqlite cascade: bad parent table %q", parent)
	}
	// Predicate stability: a SET NULL edge on the parent itself changes parent
	// rows before the parent delete re-evaluates where. Refuse a where that
	// names such a column (a conservative substring check) rather than delete
	// a different set than was asked for. Retention design §6.2 C2.
	for _, e := range SQLiteCascadeGraph {
		if e.Child == parent && e.Parent == parent && e.Action == FKSetNull && strings.Contains(where, e.Column) {
			return 0, fmt.Errorf("sqlite cascade: predicate on %s reads %s, which the cascade nulls first", parent, e.Column)
		}
	}
	sel := `SELECT id FROM ` + parent + ` WHERE ` + where
	if err := sqliteApplyEdges(ctx, q, parent, sel, args, map[string]bool{parent: true}); err != nil {
		return 0, err
	}
	res, err := q.ExecContext(ctx, `DELETE FROM `+parent+` WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("delete %s: %w", parent, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected for %s: %w", parent, err)
	}
	return n, nil
}

// SQLiteDeleteCascadingTx is sqliteDeleteCascading in its own transaction:
// on a pool it begins one (BEGIN IMMEDIATE on this driver) and commits only
// when every statement succeeded; on a handle that is already a transaction
// it runs there and the outer caller owns commit/rollback.
func SQLiteDeleteCascadingTx(ctx context.Context, db DBTX, parent, where string, args ...any) (int64, error) {
	tx, ok, err := BeginTx(ctx, db, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlite cascade: begin: %w", err)
	}
	if !ok {
		return sqliteDeleteCascading(ctx, db, parent, where, args...)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := sqliteDeleteCascading(ctx, tx, parent, where, args...)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlite cascade: commit: %w", err)
	}
	return n, nil
}

// sqliteApplyEdges applies parent's edges to the children of the rows sel
// selects (sel yields parent ids). path holds the tables on the current
// cascade chain, so a cascade cycle is refused instead of recursing forever.
func sqliteApplyEdges(ctx context.Context, q DBTX, parent, sel string, args []any, path map[string]bool) error {
	for _, e := range SQLiteCascadeGraph {
		if e.Parent != parent || e.PostgresOnly {
			continue
		}
		if !sqlIdent.MatchString(e.Child) || !sqlIdent.MatchString(e.Column) {
			return fmt.Errorf("sqlite cascade: bad identifier in edge %s.%s", e.Child, e.Column)
		}
		match := e.Column + ` IN (` + sel + `)`
		switch e.Action {
		case FKSetNull:
			if _, err := q.ExecContext(ctx, `UPDATE `+e.Child+` SET `+e.Column+` = NULL WHERE `+match, args...); err != nil {
				return fmt.Errorf("cascade %s.%s set null: %w", e.Child, e.Column, err)
			}
		case FKCascade:
			if path[e.Child] {
				return fmt.Errorf("sqlite cascade: cycle through %s.%s", e.Child, e.Column)
			}
			path[e.Child] = true
			if err := sqliteApplyEdges(ctx, q, e.Child, `SELECT id FROM `+e.Child+` WHERE `+match, args, path); err != nil {
				return err
			}
			delete(path, e.Child)
			if _, err := q.ExecContext(ctx, `DELETE FROM `+e.Child+` WHERE `+match, args...); err != nil {
				return fmt.Errorf("cascade %s.%s delete: %w", e.Child, e.Column, err)
			}
		default:
			return fmt.Errorf("sqlite cascade: edge %s.%s has no action", e.Child, e.Column)
		}
	}
	return nil
}
