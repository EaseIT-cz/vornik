package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"
)

// SQLite connect deadline (storage-abstraction design, 2026-09-24): Connect
// imposed a fixed 5 s open+ping bound whatever the caller granted, so
// `vornikctl doctor --offline` (30 s) and three internal/cli gate tests failed
// on a saturated host at 5.8 s and 6.7 s. The caller's budget now wins.
func TestConnectContext_TheCallersBudgetWins(t *testing.T) {
	remaining := func(ctx context.Context) time.Duration {
		d, ok := ctx.Deadline()
		if !ok {
			t.Fatal("derived context has no deadline; an open must always be bounded")
		}
		return time.Until(d)
	}
	near := func(got, want time.Duration) bool { return got <= want && got > want-500*time.Millisecond }

	t.Run("no timeout, no caller deadline: the 5s default", func(t *testing.T) {
		ctx, cancel, bound := connectContext(context.Background(), 0)
		defer cancel()
		if got := remaining(ctx); !near(got, 5*time.Second) {
			t.Errorf("bound = %v, want ~5s", got)
		}
		if !strings.Contains(bound, "default") {
			t.Errorf("bound label %q does not say default", bound)
		}
	})
	t.Run("no timeout, caller deadline 30s: the caller's 30s", func(t *testing.T) {
		parent, pc := context.WithTimeout(context.Background(), 30*time.Second)
		defer pc()
		ctx, cancel, bound := connectContext(parent, 0)
		defer cancel()
		if got := remaining(ctx); !near(got, 30*time.Second) {
			t.Errorf("bound = %v, want ~30s — the caller's deadline, not a second shorter one", got)
		}
		if !strings.Contains(bound, "caller deadline") {
			t.Errorf("bound label %q does not say caller deadline", bound)
		}
	})
	t.Run("configured timeout shorter than the caller: the configured one", func(t *testing.T) {
		parent, pc := context.WithTimeout(context.Background(), 30*time.Second)
		defer pc()
		ctx, cancel, bound := connectContext(parent, 3*time.Second)
		defer cancel()
		if got := remaining(ctx); !near(got, 3*time.Second) {
			t.Errorf("bound = %v, want ~3s", got)
		}
		if !strings.Contains(bound, "ConnectTimeout") {
			t.Errorf("bound label %q does not say ConnectTimeout", bound)
		}
	})
	t.Run("configured timeout longer than the caller: the caller's shorter one", func(t *testing.T) {
		parent, pc := context.WithTimeout(context.Background(), 4*time.Second)
		defer pc()
		ctx, cancel, bound := connectContext(parent, 20*time.Second)
		defer cancel()
		if got := remaining(ctx); !near(got, 4*time.Second) {
			t.Errorf("bound = %v, want ~4s — a derived context never outlives its caller", got)
		}
		if !strings.Contains(bound, "from caller deadline") {
			t.Errorf("bound label %q names ConnectTimeout, but the caller's 4s is what fires", bound)
		}
	})
}

// The ping error names the bound it ran out of, so the next timeout says whose
// budget it was without a debug log (design F6).
func TestConnect_PingErrorNamesItsBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // an already-done caller context: the ping fails at once
	_, err := Connect(ctx, Config{Path: t.TempDir() + "/v.db"})
	if err == nil {
		t.Fatal("Connect on a cancelled context succeeded")
	}
	if !strings.Contains(err.Error(), "bound:") {
		t.Errorf("ping error does not name its bound: %v", err)
	}
}
