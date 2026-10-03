package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"
)

// SQLite fixture flake (storage-abstraction design, "SQLite test fixtures",
// 2026-10-03): test fixtures opened with a 5 s bound and timed out under
// parallel `make test` load. The fixture configs carry the 60 s test bound; the
// daemon's default (no ConnectTimeout, no caller deadline) stays 5 s.
func TestFixtureConfig_CarriesTheTestBound_ProductionDefaultUnchanged(t *testing.T) {
	if FixtureConnectTimeout != 60*time.Second {
		t.Fatalf("FixtureConnectTimeout = %v, want 60s (the design's stated bound)", FixtureConnectTimeout)
	}
	if got := FixtureConfig("/x/f.db"); got.ConnectTimeout != FixtureConnectTimeout || got.Path != "/x/f.db" || got.MaxOpenConns != 0 {
		t.Fatalf("FixtureConfig = %+v, want the path, the fixture bound and Connect's default pool", got)
	}
	if got := DefaultConfig(); got.ConnectTimeout != FixtureConnectTimeout || got.Path != ":memory:" || got.MaxOpenConns != 1 {
		t.Fatalf("DefaultConfig = %+v, want :memory:, pool 1 and the fixture bound", got)
	}
	if defaultConnectTimeout != 5*time.Second {
		t.Fatalf("the production default moved to %v; this change must leave it at 5s", defaultConnectTimeout)
	}

	ctx, cancel, bound := connectContext(context.Background(), FixtureConfig("f.db").ConnectTimeout)
	defer cancel()
	d, ok := ctx.Deadline()
	if !ok || time.Until(d) < 59*time.Second {
		t.Fatalf("fixture open deadline = %v (ok=%v), want ~60s", time.Until(d), ok)
	}
	if !strings.Contains(bound, "1m0s from ConnectTimeout") {
		t.Fatalf("bound label %q", bound)
	}
}
