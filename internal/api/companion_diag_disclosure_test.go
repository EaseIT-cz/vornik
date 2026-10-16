package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/postmortem"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// Security scan (information disclosure) of companion_diag_tools.go, GitHub
// #92 (2026-10-08): daemon_log returned the daemon's free-text log message,
// scrubbed only for secrets. A broker project's line can carry the task's
// raw error or step output that status, result and task_logs withhold, and
// any line can name another project, server URL, path or host. Every agent
// namespace project is a broker project, so daemon_log now returns closed
// fields only, plus the message when it is one of an allowlist of the
// daemon's own literal messages (which cannot carry content).
func TestDaemonLog_BrokerLineNeverCarriesFreeText(t *testing.T) {
	f := newDiagFixture(t)
	f.tasks["t1"] = &persistence.Task{ID: "t1", ProjectID: "hermes--fin"}
	f.logLine(`"component":"executor","project":"hermes--fin","task_id":"t1","message":"step digest: Dear Bob, your invoice from other--x at https://mail.internal/x"`)
	out, text, isErr := f.call(t, f.adminRaw, "daemon_log", map[string]any{})
	require.False(t, isErr, text)
	lines := diagLines(out)
	require.Len(t, lines, 1)
	assert.NotContains(t, text, "Dear Bob")
	assert.NotContains(t, text, "other--x")
	assert.NotContains(t, text, "mail.internal")
	_, has := lines[0]["message"]
	assert.False(t, has)
	assert.Equal(t, true, lines[0]["message_omitted"])
	assert.Equal(t, "t1", lines[0]["task_id"], "closed fields stay")
}

// The other direction: a message that is one of the daemon's own literals
// (here the MCP connect failure) is returned, with its closed code.
func TestDaemonLog_AllowlistedLiteralMessageKept(t *testing.T) {
	f := newDiagFixture(t)
	f.logLine(`"component":"mcp","project":"hermes--fin","server":"mail","http_status":403,"message":"mcp: failed to connect"`)
	out, text, isErr := f.call(t, f.adminRaw, "daemon_log", map[string]any{})
	require.False(t, isErr, text)
	lines := diagLines(out)
	require.Len(t, lines, 1)
	assert.Equal(t, "mcp: failed to connect", lines[0]["message"])
	assert.Equal(t, "auth_rejected", lines[0]["code"])
}

// The allowlist can only hold the daemon's own literals: every entry appears
// verbatim as a Msg("...") argument in non-test source, so no entry can be
// content.
func TestDaemonLogMessageAllowlist_OnlyCodeLiterals(t *testing.T) {
	var src strings.Builder
	err := filepath.WalkDir("../../internal", func(path string, d fs.DirEntry, werr error) error {
		if werr == nil && d.IsDir() && repowalk.SkipDir("../../internal", path, d.Name()) {
			return filepath.SkipDir
		}
		if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr == nil {
			src.Write(b)
		}
		return nil
	})
	require.NoError(t, err)
	all := src.String()
	require.NotEmpty(t, daemonLogMessageAllowlist)
	for msg := range daemonLogMessageAllowlist {
		assert.Contains(t, all, `Msg("`+msg+`")`, "allowlisted message is not a Msg literal in the daemon")
	}
}

// Security scan (information disclosure), item 3, GitHub #92 (2026-10-08):
// task_explain for a non-broker task in the caller's own project discloses
// nothing beyond what that key already reads through status (its own raw
// last_error) and the companion live view (its own events, scrubbed): no
// tool input or output bodies, no step error detail beyond the class, and
// every string scrubbed.
func TestTaskExplain_CompanionViewHoldsOnlyClosedStepFields(t *testing.T) {
	f := newDiagFixture(t)
	cls := persistence.TaskFailureClassToolError
	f.tasks["t-a"] = &persistence.Task{ID: "t-a", ProjectID: "alpha", Status: persistence.TaskStatusFailed,
		LastError: strp("tool failed with " + plantedToken), LastErrorClass: &cls}
	f.srv.explainRenderer = postmortem.NewRenderer(f.srv.taskRepo, nil, nil, nil, failingTailFetcher{t})
	out, text, isErr := f.call(t, f.plainRaw, "task_explain", map[string]any{"task_id": "t-a"})
	require.False(t, isErr, text)
	// review-20261008-33a5 F1, F2: the answer is built from an allowlist; pin
	// the exact key set, and the secret in last_error is withheld.
	keys := []string{}
	for k := range out {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"task_id", "task_status", "workflow_id", "last_error_class", "last_error",
		"step_outcomes", "playbook"}, keys)
	assert.Equal(t, "[withheld: secret]", out["last_error"])
	assert.NotContains(t, text, "ghp_")
	for _, k := range []string{"recent_tools", "log_tail", "summary", "error_detail"} {
		assert.NotContains(t, text, `"`+k+`"`)
	}
}
