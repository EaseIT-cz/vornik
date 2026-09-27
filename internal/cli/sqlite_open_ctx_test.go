package cli

import (
	"context"
	"testing"
	"time"
)

// sqliteOpenCtx is the context for a test that opens a real SQLite file. It
// carries a deadline, as a CLI command's context does (doctor --offline grants
// 30 s), so the open is bounded by it rather than by sqlite.Connect's 5 s
// no-deadline default — which a saturated host's first open of a fresh file
// (WAL creation + fsync under synchronous(FULL)) exceeded at 5.8 s and 6.7 s,
// failing these tests as "ping: context deadline exceeded" (2026-09-23/24;
// storage-abstraction design, SQLite connect deadline).
func sqliteOpenCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}
