package api

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// GitHub #389 (memory-ingest-paths.md §8.2e): the operator sweep for damaged
// chunk text. The production sweep on 2026-10-16 examined 86,007 chunks and
// found 3 with C0 control bytes; a doctor check makes that repeatable and
// publishes the denominator so "0 damaged" can never mean "0 examined".

func memoryChunksDB(t *testing.T, rows [][4]string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE project_memory_chunks (
		id TEXT PRIMARY KEY, project_id TEXT NOT NULL, content TEXT NOT NULL DEFAULT '',
		validation_status TEXT NOT NULL DEFAULT 'unverified',
		lifecycle_state TEXT NOT NULL DEFAULT 'published')`)
	require.NoError(t, err)
	for _, r := range rows {
		// id, content, validation_status, lifecycle_state
		_, err := db.Exec(`INSERT INTO project_memory_chunks (id, project_id, content, validation_status, lifecycle_state)
			VALUES (?, 'p1', ?, ?, ?)`, r[0], r[1], r[2], r[3])
		require.NoError(t, err)
	}
	return db
}

func TestMemoryTextDamage_NoDatabaseIsSkipped(t *testing.T) {
	c := NewDoctorHandlers(nil).checkMemoryTextDamage(context.Background())
	require.Equal(t, "SKIPPED", c.Status)
}

func TestMemoryTextDamage_EmptyTableIsSkippedNotOK(t *testing.T) {
	c := NewDoctorHandlers(memoryChunksDB(t, nil)).checkMemoryTextDamage(context.Background())
	require.Equal(t, "SKIPPED", c.Status, "0 examined is no evidence: %s", c.Message)
	require.Contains(t, c.Message, "0")
}

func TestMemoryTextDamage_CleanCorpusIsOKAndPublishesTheDenominator(t *testing.T) {
	db := memoryChunksDB(t, [][4]string{
		{"a", "plain prose\n", "unverified", "published"},
		{"b", "tab\tand\fformfeed", "verified", "published"},
		{"c", "a quoted � mark", "unverified", "published"},
	})
	c := NewDoctorHandlers(db).checkMemoryTextDamage(context.Background())
	require.Equal(t, "OK", c.Status, c.Message)
	require.Contains(t, c.Message, "0 of 3 chunks examined")
	require.Contains(t, c.Message, "1 carry U+FFFD", "the repair-mark count is published as information")
}

func TestMemoryTextDamage_LiveDamageWarnsWithIDs(t *testing.T) {
	db := memoryChunksDB(t, [][4]string{
		{"clean", "plain", "unverified", "published"},
		{"277f4def", "Example: https://telemety.vornik.io\x0b݌K", "refuted", "published"},
		{"bcef9e73", "## 8. Test plan\x1b\x02", "unverified", "published"},
		{"old", "superseded\x01", "unverified", "superseded"},
	})
	c := NewDoctorHandlers(db).checkMemoryTextDamage(context.Background())
	require.Equal(t, "WARNING", c.Status, c.Message)
	require.Contains(t, c.Message, "3 of 4 chunks examined hold damaged text (1 live")
	require.Len(t, c.Items, 1+1, "one live id plus the remedy line")
	require.True(t, strings.HasPrefix(c.Items[0], "bcef9e73 "), "items name the live chunk id: %v", c.Items)
	require.NotContains(t, strings.Join(c.Items, "\n"), "Test plan", "items carry ids, never content")
}

func TestMemoryTextDamage_OnlyRefutedDamageIsOK(t *testing.T) {
	db := memoryChunksDB(t, [][4]string{
		{"277f4def", "x\x0by", "refuted", "published"},
		{"clean", "plain", "unverified", "published"},
	})
	c := NewDoctorHandlers(db).checkMemoryTextDamage(context.Background())
	require.Equal(t, "OK", c.Status, c.Message)
	require.Contains(t, c.Message, "1 of 2 chunks examined hold damaged text (0 live")
}

func TestMemoryTextDamage_DeadlineIsAWarningWithThePartialDenominator(t *testing.T) {
	db := memoryChunksDB(t, [][4]string{
		{"a", "plain", "unverified", "published"},
		{"b", "plain", "unverified", "published"},
	})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	c := NewDoctorHandlers(db).checkMemoryTextDamage(ctx)
	require.Equal(t, "WARNING", c.Status, "an unfinished scan is never OK: %s", c.Message)
	require.Contains(t, c.Message, "examined 0 of", "the partial verdict publishes how much was examined")
}

func TestMemoryTextDamage_PartialScanVerdict(t *testing.T) {
	c := evaluateMemoryTextDamage(memoryTextDamageScan{Total: 10, Examined: 4}, true)
	require.Equal(t, "WARNING", c.Status)
	require.Contains(t, c.Message, "examined 4 of 10")
}
