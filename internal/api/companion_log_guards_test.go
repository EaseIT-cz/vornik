package api

import (
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #92 (2026-10-08), live design Change C8: no process spawn. The files
// of the agent log path import nothing that can exec a program; the package
// as a whole reaches the executor, so the guard is per file. (Since GitHub
// #294 the server holds no container-log source at all: every log read is
// HeldTaskLog, and TestSpawnLaw_ContainerLogsReadOnlyOnStepFailure pins it.)
func TestLiveLogFiles_NoProcessSpawnImports(t *testing.T) {
	forbidden := []string{
		"os/exec",
		"vornik.io/vornik/internal/spawn",
		"vornik.io/vornik/internal/runtime",
		"vornik.io/vornik/internal/executor",
	}
	files := []string{"companion_task_logs.go", "live_projection.go", "live_logs_metrics.go"}
	for _, f := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		require.NoError(t, err, f)
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				// internal/executor/livepubsub is the event vocabulary, not the
				// executor; only the executor package itself is refused.
				if path == bad || (strings.HasPrefix(path, bad+"/") && bad != "vornik.io/vornik/internal/executor") {
					t.Errorf("%s imports %s", f, path)
				}
			}
		}
	}
}

// GitHub #92 (2026-10-08), live design Change C6c: the audit row of a
// task_logs call holds the size and a sha256 of what left, never the content.
func TestCompanionAuditOutput_TaskLogsHasDigestNotContent(t *testing.T) {
	out := companionAuditOutput("task_logs", "ok", `{"events":[{"kind":"step_started"}]}`)
	assert.Contains(t, out, "sha256=")
	assert.Contains(t, out, "result_bytes=")
	assert.NotContains(t, out, "step_started")
}

// audit 2026-10-12 (finding C): broker design §6b.6 says the delegate audit
// row's output carries `reused`; it carried only status and size, so an
// operator could not tell a reused answer from a fresh run in the audit.
func TestCompanionAuditOutput_DelegateRecordsReused(t *testing.T) {
	assert.Contains(t, companionAuditOutput("delegate", "ok", `{"task_id":"t1","reused":true,"as_of":"x"}`), " reused=true")
	assert.Contains(t, companionAuditOutput("delegate", "ok", `{"task_id":"t2","status":"QUEUED"}`), " reused=false")
	assert.Contains(t, companionAuditOutput("delegate", "error: REUSE_NOT_ALLOWED", ""), " reused=false")
	assert.NotContains(t, companionAuditOutput("status", "ok", `{"reused":true}`), "reused=")
}
