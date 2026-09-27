package cli

import (
	"testing"
	"time"
)

// The harness waited a fixed 30 minutes per task (agentbench.DaemonConfig's
// default) with no way to change it. On the slow-hardware Ollama arm
// (2026-09-25) a dev-pipeline task with its step budgets raised cannot finish
// in 30 minutes, so the harness would record every such task as a timeout
// regardless of what the daemon did. VORNIK_BENCH_TASK_TIMEOUT overrides it.
func TestBenchTaskTimeout(t *testing.T) {
	t.Setenv("VORNIK_BENCH_TASK_TIMEOUT", "")
	if got, err := benchTaskTimeout(); err != nil || got != 0 {
		t.Fatalf("unset: %v %v, want 0 (the runner's own default)", got, err)
	}
	t.Setenv("VORNIK_BENCH_TASK_TIMEOUT", "4h")
	if got, err := benchTaskTimeout(); err != nil || got != 4*time.Hour {
		t.Fatalf("4h: %v %v", got, err)
	}
	for _, bad := range []string{"soon", "-5m", "0s"} {
		t.Setenv("VORNIK_BENCH_TASK_TIMEOUT", bad)
		if _, err := benchTaskTimeout(); err == nil {
			t.Errorf("%q must be refused, not silently replaced by the default", bad)
		}
	}
}
