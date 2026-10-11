package service

import (
	"testing"

	"vornik.io/vornik/internal/ui"
)

// TestUIExecutorAdapter_IsNotALogSource — GitHub #294 (2026-10-08). The
// adapter used to forward TaskLogs to the executor so ui.WithExecutor would
// wire it as the log panel's source (the 2026-05-23 empty-panel fix). That
// ran `podman logs` every 2 s per open panel: a request-triggered spawn. The
// panel's source is now the daemon-held task log, wired with
// ui.WithTaskLogSource in initHTTPServer; the e2e
// TestSpawnLaw_TaskLogRoutesRunNoPodman proves the panel still shows the
// task's log on the real binary (so the 2026-05-23 regression stays covered).
func TestUIExecutorAdapter_IsNotALogSource(t *testing.T) {
	var a any = uiExecutorAdapter{}
	if _, ok := a.(ui.TaskLogSource); ok {
		t.Fatal("uiExecutorAdapter implements ui.TaskLogSource again: the UI log panel would run podman logs (GitHub #294)")
	}
}

// TestUIExecutorAdapter_ImplementsExecutorInterface — the compile-time
// assertion the wire site makes, stated as a test so a method added to
// ui.ExecutorInterface without an adapter forward is named here rather
// than as a typecheck error inside make lint (2026-09-04, RetryFromStep).
func TestUIExecutorAdapter_ImplementsExecutorInterface(t *testing.T) {
	var a any = uiExecutorAdapter{}
	if _, ok := a.(ui.ExecutorInterface); !ok {
		t.Fatal("uiExecutorAdapter must implement ui.ExecutorInterface")
	}
}
