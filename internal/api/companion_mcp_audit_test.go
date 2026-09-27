package api

// B-17: companion-plugin tool calls roll up into tool_audit_log so
// operators see them in the same "everything I called" view that
// agent-side tool calls land in. These tests pin the row shape +
// the nil-safe "no repo wired" branch.

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// fakeToolAuditRepo captures every Log call so tests can assert on
// the row shape without needing a real DB.
type fakeToolAuditRepo struct {
	mu      sync.Mutex
	entries []*persistence.ToolAuditEntry
}

func (f *fakeToolAuditRepo) Log(_ context.Context, e *persistence.ToolAuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *e
	f.entries = append(f.entries, &cp)
	return nil
}
func (f *fakeToolAuditRepo) List(_ context.Context, _ persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return nil, nil
}
func (f *fakeToolAuditRepo) ToolLatencyP95ByProjectTool(_ context.Context, _ time.Time) ([]persistence.ToolLatencyStat, error) {
	return nil, nil
}
func (f *fakeToolAuditRepo) CountByTool(_ context.Context, _ string) (map[string]int64, error) {
	return nil, nil
}

// snapshot returns a copy of captured entries so the test can read
// outside the lock.
func (f *fakeToolAuditRepo) snapshot() []*persistence.ToolAuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*persistence.ToolAuditEntry, len(f.entries))
	copy(out, f.entries)
	return out
}

// TestCompanionMCP_ToolAudit_RollupCapturesCallShape — happy path:
// a catalog call lands exactly one tool_audit_log row with the
// expected fields. Catalog is chosen because it takes no
// arguments + doesn't require any external state; the audit row
// shape is the same for every tool.
func TestCompanionMCP_ToolAudit_RollupCapturesCallShape(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, row := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	audit := &fakeToolAuditRepo{}
	srv.toolAuditRepo = audit

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "catalog",
		"arguments": map[string]any{},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	entries := audit.snapshot()
	require.Len(t, entries, 1, "catalog must produce exactly one tool_audit_log row")
	e := entries[0]
	assert.Equal(t, "mcp__plugin_vornik-companion_vornik__catalog", e.ToolName)
	assert.Equal(t, row.ProjectID, e.ProjectID, "row scoped to the key's project")
	assert.Equal(t, "companion:"+row.ID, e.TaskID,
		"task_id synthesised from the API key so rows group by operator session")
	assert.NotEmpty(t, e.ExecutionID, "execution_id is generated per call")
	assert.True(t, strings.HasPrefix(e.ExecutionID, "compex_"),
		"execution_id uses the compex_ prefix")
	assert.Contains(t, e.ToolOutput, "status=ok",
		"tool_output records a successful outcome")
	assert.GreaterOrEqual(t, e.DurationMs, int64(0))
}

// TestCompanionMCP_ToolAudit_RollupRecordsErrorOutcome — a call
// that fails (cross-project status lookup) still lands an audit
// row, with the error captured in tool_output. Without this, a
// failed companion call would be invisible to the operator's
// audit dashboards.
func TestCompanionMCP_ToolAudit_RollupRecordsErrorOutcome(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		// Task owned by a different project — the handler returns
		// "task not found" so the bearer can't probe other tenants.
		return &persistence.Task{
			ID: "task-leaks", ProjectID: "beta",
			Status: persistence.TaskStatusRunning,
		}, nil
	}
	audit := &fakeToolAuditRepo{}
	srv.toolAuditRepo = audit

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "status",
		"arguments": map[string]any{"task_id": "task-leaks"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	entries := audit.snapshot()
	require.Len(t, entries, 1)
	e := entries[0]
	assert.Equal(t, "mcp__plugin_vornik-companion_vornik__status", e.ToolName)
	assert.Contains(t, e.ToolOutput, "status=error",
		"failed calls record their outcome — operators see them in /ui/admin/audit")
}

// TestCompanionMCP_ToolAudit_NoRepoSafe — the audit roll-up is
// best-effort. When toolAuditRepo isn't wired the handler must
// continue to work without panic.
func TestCompanionMCP_ToolAudit_NoRepoSafe(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	// No toolAuditRepo set.

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "catalog",
		"arguments": map[string]any{},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	require.NotPanics(t, func() { srv.CompanionMCPHandler(rec, req) })
}

// TestCompanionMCP_ToolAudit_DoesNotIncludeArgumentContent — the
// row's tool_input must capture only a request-shape summary, not
// the raw arguments. A remember() can carry 64 KiB of secrets and
// the audit table is the wrong place to mirror that. We pin the
// "byte length only" shape so a future refactor doesn't quietly
// spill content.
func TestCompanionMCP_ToolAudit_DoesNotIncludeArgumentContent(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	audit := &fakeToolAuditRepo{}
	srv.toolAuditRepo = audit

	secret := "extremely-sensitive-content-AAAAAAAAAA"
	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "catalog",
		"arguments": map[string]any{"unused": secret},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	entries := audit.snapshot()
	require.Len(t, entries, 1)
	e := entries[0]
	assert.NotContains(t, e.ToolInput, secret,
		"tool_input must NOT echo argument content — only size")
	assert.Contains(t, e.ToolInput, "args_bytes=",
		"tool_input records the byte length")
}

// Companion tool-audit design (2026-09-24): the writer left ID and CreatedAt
// unset for four months, so the first row took id ” and every later call was
// absorbed by ON CONFLICT (id), and Postgres dated the survivor year 1 for the
// retention sweep to delete. Production held zero companion rows. The shape
// test above never looked at either field.
func TestCompanionMCP_ToolAudit_EveryCallGetsItsOwnIDAndTime(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	audit := &fakeToolAuditRepo{}
	srv.toolAuditRepo = audit
	before := time.Now().UTC().Add(-time.Second)
	for i := 0; i < 2; i++ {
		req := withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{"name": "catalog", "arguments": map[string]any{}}), raw)
		srv.CompanionMCPHandler(httptest.NewRecorder(), req)
	}
	entries := audit.snapshot()
	require.Len(t, entries, 2)
	assert.NotEmpty(t, entries[0].ID)
	assert.NotEmpty(t, entries[1].ID)
	assert.NotEqual(t, entries[0].ID, entries[1].ID, "two calls must be two rows, not one collision")
	for _, e := range entries {
		assert.False(t, e.CreatedAt.Before(before), "CreatedAt must be the call time, got %v", e.CreatedAt)
	}
}

// Companion tool-audit design F1: the outcome COLUMN is set, not only folded
// into tool_output — the audit views classify rows on it.
func TestCompanionMCP_ToolAudit_SetsTheOutcomeColumn(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	audit := &fakeToolAuditRepo{}
	srv.toolAuditRepo = audit
	req := withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{"name": "catalog", "arguments": map[string]any{}}), raw)
	srv.CompanionMCPHandler(httptest.NewRecorder(), req)
	entries := audit.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, "ok", entries[0].Outcome)
	assert.Contains(t, entries[0].ToolOutput, "status=ok")
}

// Implementation review F1: a tool error sets the column to "error", and the
// status text in tool_output agrees with it.
func TestCompanionMCP_ToolAudit_SetsTheOutcomeColumnOnError(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{ID: "task-other", ProjectID: "beta", Status: persistence.TaskStatusRunning}, nil
	}
	audit := &fakeToolAuditRepo{}
	srv.toolAuditRepo = audit
	req := withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{"name": "status", "arguments": map[string]any{"task_id": "task-other"}}), raw)
	srv.CompanionMCPHandler(httptest.NewRecorder(), req)
	entries := audit.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, "error", entries[0].Outcome)
	assert.Contains(t, entries[0].ToolOutput, "status=error")
}
