package watchdog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestMarkFailed_ReasonSaysWhatToDo — audit 2026-10-12 (GitHub #142,
// outcome-quality LLD §S1 R1): the stuck reason said only "no checkpoint
// advance in D". It did not say what can stop an execution advancing — a hung
// model call, a hung container, a wedged executor — nor where to look first,
// so the operator's next step was a guess. The prefix stays (logs and
// searches key on it); the remedy is appended.
func TestMarkFailed_ReasonSaysWhatToDo(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{}
	cfg := DefaultConfig()
	cfg.Action = ActionFail
	w := New(cfg, exec, newStubTaskRepo(), zerolog.Nop(), nil)
	w.now = func() time.Time { return now }

	require.NoError(t, w.markFailed(context.Background(), runningExec("e1", "", "p1", now, 45*time.Minute), 45*time.Minute))
	f := exec.failures()
	require.Len(t, f, 1)
	msg := f[0].message
	for _, want := range []string{
		"watchdog: execution stuck — no checkpoint advance in 45m0s", // kept prefix
		"model call",       // names the likely causes
		"container log",    // where to look
		"vornikctl doctor", // the tool to run
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("stuck reason %q does not contain %q", msg, want)
		}
	}
}
