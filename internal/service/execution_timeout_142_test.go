package service

import (
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
)

// audit 2026-10-12: outcome-quality §S3 #1 (GitHub #142) — the scheduler's
// ExecutionTimeout was configured (30m) and never read, and the daemon YAML
// could not reach it. 05-scheduler §4.12 C1: scheduler.execution_timeout feeds
// the executor's default wall-clock cap; empty or invalid means no cap, the
// behaviour before this change.
func TestDefaultMaxWallClock_FromSchedulerExecutionTimeout(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want time.Duration
	}{
		{"", 0},
		{"6h", 6 * time.Hour},
		{"soon", 0},
		{"-1m", 0},
	} {
		ct := &Container{Config: &config.Config{}, Logger: zerolog.Nop()}
		ct.Config.Scheduler.ExecutionTimeout = c.raw
		if got := ct.defaultMaxWallClock(); got != c.want {
			t.Errorf("execution_timeout %q → %s, want %s", c.raw, got, c.want)
		}
	}
}
