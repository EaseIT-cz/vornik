package mcp

import (
	"errors"
	"testing"
	"time"
)

// GitHub #92 (2026-10-08), live design Change C7: the doctor tool classifies
// why a project's MCP server is not connected from the failure's type and
// HTTP status, never its text, so the pending view carries both.
func TestPendingServer_CarriesCauseAndStatus(t *testing.T) {
	cause := &CallError{Server: "mail", Status: 401}
	p := newPendingServer(ServerConfig{Name: "mail"}, cause, time.Now())
	m := &Manager{pending: map[string]map[string]*pendingServer{"proj": {"mail": p}}, clients: map[string]map[string]*Client{}}
	_, pending := m.PendingStatus()
	if len(pending) != 1 {
		t.Fatalf("want one pending server, got %d", len(pending))
	}
	if pending[0].HTTPStatus != 401 {
		t.Errorf("HTTPStatus = %d, want 401", pending[0].HTTPStatus)
	}
	var ce *CallError
	if !errors.As(pending[0].Cause, &ce) {
		t.Errorf("Cause lost its type: %v", pending[0].Cause)
	}
}
