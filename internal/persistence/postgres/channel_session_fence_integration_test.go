//go:build integration

package postgres

import (
	"context"
	"testing"
)

// The channel_sessions version fence, against a real database.
//
// GitHub #344 slice 5d-2 (art17 redact design §15.18). Found at: review (the
// as-built §15.10.1): a stored session deleted by an erasure was written back
// by the daemon's next blind upsert, history and all, while the report said
// "deleted". Every write now draws a version from one sequence, and the
// daemon saves only over the version it last saw.

func fenceRepo(t *testing.T) (*DB, *ChannelSessionRepository) {
	t.Helper()
	db := newIntegrationDB(t)
	clean := func() {
		if _, err := db.DB.ExecContext(context.Background(),
			`DELETE FROM channel_sessions WHERE session_id LIKE 'fence-%'`); err != nil {
			t.Fatalf("reset: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)
	return db, NewChannelSessionRepository(db.DB)
}

func TestChannelSessionFence_SaveIfVersion(t *testing.T) {
	ctx := context.Background()
	_, repo := fenceRepo(t)

	found, _, err := repo.Version(ctx, "slack", "fence-1")
	if err != nil || found {
		t.Fatalf("missing row: found=%v err=%v", found, err)
	}
	v1, ok, err := repo.SaveIfVersion(ctx, "slack", "fence-1", "p", []byte(`[{"role":"user","content":"a"}]`), nil)
	if err != nil || !ok || v1 == 0 {
		t.Fatalf("insert while absent: v=%d ok=%v err=%v", v1, ok, err)
	}
	// Absent was expected, but the row exists now: refused.
	if _, ok, err := repo.SaveIfVersion(ctx, "slack", "fence-1", "p", []byte(`[]`), nil); err != nil || ok {
		t.Fatalf("insert over an existing row: ok=%v err=%v", ok, err)
	}
	v2, ok, err := repo.SaveIfVersion(ctx, "slack", "fence-1", "p", []byte(`[{"role":"user","content":"b"}]`), &v1)
	if err != nil || !ok || v2 == v1 {
		t.Fatalf("update over v1: v=%d ok=%v err=%v", v2, ok, err)
	}
	// A writer still holding v1 is refused, and writes nothing.
	if _, ok, err := repo.SaveIfVersion(ctx, "slack", "fence-1", "p", []byte(`[{"role":"user","content":"stale"}]`), &v1); err != nil || ok {
		t.Fatalf("stale update: ok=%v err=%v", ok, err)
	}
	row, err := repo.Load(ctx, "slack", "fence-1")
	if err != nil || row.Version != v2 || string(row.History) != `[{"role": "user", "content": "b"}]` {
		t.Fatalf("after the refused write: %+v, %v", row, err)
	}
	found, v, err := repo.Version(ctx, "slack", "fence-1")
	if err != nil || !found || v != v2 {
		t.Fatalf("Version = %v %d %v, want %d", found, v, err, v2)
	}
}

// A delete and a re-create never reuse a version (ABA), and a write that does
// not go through SaveIfVersion (a binary predating the fence) still moves it.
func TestChannelSessionFence_VersionsNeverRepeatAndEveryWriteMovesThem(t *testing.T) {
	ctx := context.Background()
	db, repo := fenceRepo(t)

	v1, _, err := repo.SaveIfVersion(ctx, "email", "fence-2", "", []byte(`[]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, "email", "fence-2"); err != nil {
		t.Fatal(err)
	}
	v2, ok, err := repo.SaveIfVersion(ctx, "email", "fence-2", "", []byte(`[]`), nil)
	if err != nil || !ok || v2 == v1 {
		t.Fatalf("re-created: v=%d (was %d) ok=%v err=%v", v2, v1, ok, err)
	}
	// The pre-fence upsert, verbatim.
	if _, err := db.DB.ExecContext(ctx, `
INSERT INTO channel_sessions (kind, session_id, active_project, history, created_at, updated_at)
VALUES ('email', 'fence-2', NULL, '[{"role":"user","content":"old binary"}]', NOW(), NOW())
ON CONFLICT (kind, session_id) DO UPDATE
SET active_project = EXCLUDED.active_project, history = EXCLUDED.history, updated_at = NOW()`); err != nil {
		t.Fatal(err)
	}
	_, v3, err := repo.Version(ctx, "email", "fence-2")
	if err != nil || v3 == v2 {
		t.Fatalf("a blind upsert left the version at %d", v3)
	}
	if _, ok, _ := repo.SaveIfVersion(ctx, "email", "fence-2", "", []byte(`[]`), &v2); ok {
		t.Fatal("a writer holding the version before the blind upsert was not refused")
	}
}

// Existing rows are numbered distinctly by the migration's volatile default;
// a plain INSERT (as seeded by older tests) gets one too.
func TestChannelSessionFence_RowsGetDistinctVersions(t *testing.T) {
	ctx := context.Background()
	db, _ := fenceRepo(t)
	for _, sid := range []string{"fence-3a", "fence-3b"} {
		if _, err := db.DB.ExecContext(ctx,
			`INSERT INTO channel_sessions (kind, session_id, history) VALUES ('github', $1, '[]')`, sid); err != nil {
			t.Fatal(err)
		}
	}
	var n, distinct int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT version) FROM channel_sessions
		WHERE session_id LIKE 'fence-3%'`).Scan(&n, &distinct); err != nil {
		t.Fatal(err)
	}
	if n != 2 || distinct != 2 {
		t.Fatalf("rows %d, distinct versions %d", n, distinct)
	}
}
