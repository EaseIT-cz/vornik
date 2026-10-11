package api

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpconnect"
)

// GitHub #91 (2026-10-08), auth design §14: an oauth server with no grant is
// withheld and not dialled. The doctor names the state and the remedy the
// same way every surface does, and counts it on its own (G2: neither
// "examined clean" nor invisible).

func TestConnectorAuth_NoGrantRowIsBuilt(t *testing.T) {
	noGrant := []mcpconnect.NoGrant{
		{ProjectID: "assistant", CredentialScope: "assistant", Server: "atlassian"},
		{ProjectID: "", CredentialScope: "", Server: "linear"},
		{ProjectID: "other", CredentialScope: "", Server: "linear"}, // same pair via a name-only subscription
		{ProjectID: "hermes--venues", CredentialScope: "hermes--venues", Server: "calendar"},
		{ProjectID: "x", CredentialScope: "x", Server: "connected"}, // a grant row exists: not reported
	}
	grants := []connectorGrantState{{projectID: "x", server: "connected", hasRefresh: true}}
	items := connectorNoGrantItems(noGrant, grants)
	require.Len(t, items, 3, "%v", items)
	joined := strings.Join(items, "\n")
	for _, want := range []string{
		"not connected, no grant — fix: vornikctl mcp connect atlassian -p assistant",
		"not connected, no grant — fix: vornikctl mcp connect linear",
		"not connected, no grant — fix: sign in on the approver device",
	} {
		require.Contains(t, joined, want)
	}
	require.NotContains(t, joined, "connected\" (project x)")
}

// F1: the doctor's fix hint is the shared remedy, agent-scope aware.
func TestConnectorFixHint_IsTheSharedRemedy(t *testing.T) {
	require.Equal(t, "fix: "+mcpconnect.ConnectCommand("p", "s"), connectorFixHint("p", "s"))
	require.Contains(t, connectorFixHint("hermes--venues", "calendar"), "approver device")
}

func TestCheckMCPProjectConnections_CountsNoGrantSeparately(t *testing.T) {
	now := time.Now()
	h := doctorWithPending(3, nil, now)
	h.SetMCPNoGrantSource(func() []mcpconnect.NoGrant {
		return []mcpconnect.NoGrant{
			{ProjectID: "assistant", CredentialScope: "assistant", Server: "atlassian"},
			{ProjectID: "", Server: "linear"}, // daemon catalog: not a project server
		}
	})
	c := h.checkMCPProjectConnections()
	require.Equal(t, "WARNING", c.Status)
	require.Contains(t, c.Message, "3 project servers examined")
	require.Contains(t, c.Message, "1 withheld until connected (no OAuth grant)")
	require.Len(t, c.Items, 1)
	require.Contains(t, c.Items[0], "assistant/atlassian: not connected, no grant: run vornikctl mcp connect atlassian -p assistant")
	require.Contains(t, c.Items[0], "(withheld until connected; connecting re-wires it, no restart)")
}

// F4/F6: the servers API carries not_dialled only when true.
func TestMCPServersJSON_NotDialled(t *testing.T) {
	require.True(t, toJSON(mcp.ServerSnapshot{Name: "a", NotDialled: true, Error: "not dialled: x"}).NotDialled)
	require.False(t, toJSON(mcp.ServerSnapshot{Name: "a"}).NotDialled)
}

// Review 030f B1: checkConnectorAuth end to end, with no grants, no failures
// and one server withheld for want of a grant. It used to say OK, "no
// OAuth-backed MCP connectors are configured".
func TestCheckConnectorAuth_NoGrantOnlyIsAWarning(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	h := &DoctorHandlers{db: db}
	h.SetMCPNoGrantSource(func() []mcpconnect.NoGrant {
		return []mcpconnect.NoGrant{{ProjectID: "assistant", CredentialScope: "assistant", Server: "atlassian"}}
	})
	c := h.checkConnectorAuth(context.Background())
	require.Equal(t, "WARNING", c.Status, "%+v", c)
	require.NotContains(t, c.Message, "no OAuth-backed MCP connectors are configured")
	require.Len(t, c.Items, 1)
	require.Contains(t, c.Items[0], "not connected, no grant")
	require.Contains(t, c.Items[0], "vornikctl mcp connect atlassian -p assistant")
}
