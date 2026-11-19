package ui

// GitHub #765 (2026-07-08-control-plane-hub-design.md, Amendment
// 2026-11-18 (#765)): the managed instance's owner sees a server's sign-in
// on the project MCP tools page and disconnects it. Found at: n/a (feature;
// the owner parity census, #754). Test at: integration (the page handler
// through the auth middleware, #780's rig, a recording connector).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/persistence"
)

// oauth765 resolves "scraper" to scope (project "" = the instance's) and
// records each Disconnect's scope.
type oauth765 struct {
	cpMCPRenderOAuth
	scope        string
	mode         string
	disconnected *[]string
	fail         error
}

func (f oauth765) ResolveServer(_, name string) (mcpconnect.ServerRef, bool) {
	if name != "scraper" {
		return mcpconnect.ServerRef{}, false
	}
	return mcpconnect.ServerRef{ProjectID: f.scope, ServerName: name, Auth: mcpauth.Auth{Mode: f.mode}}, true
}

func (f oauth765) Grant(context.Context, string, string) (*persistence.MCPOAuthToken, error) {
	return &persistence.MCPOAuthToken{ConnectedBy: "session:s1", ConnectedAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}, nil
}

func (f oauth765) Disconnect(_ context.Context, projectID, server, _ string) error {
	if f.fail != nil {
		return f.fail
	}
	*f.disconnected = append(*f.disconnected, projectID+"/"+server)
	return nil
}

func rig765(t *testing.T, scope, mode string) (toolsRig780, *[]string) {
	t.Helper()
	r := newToolsRig780(t, false)
	var got []string
	WithMCPOAuthAdmin(oauth765{scope: scope, mode: mode, disconnected: &got})(r.srv)
	return r, &got
}

func disconnect765(server string) url.Values {
	return url.Values{"action": {"disconnect"}, "server": {server}}
}

func TestProjectMCPOAuth765_OwnerSeesTheSignIn(t *testing.T) {
	r, _ := rig765(t, "alpha", mcpauth.ModeOAuth)
	body := r.keys.do(t, http.MethodGet, "/projects/alpha/mcp-tools?server=scraper", owner754, nil).Body.String()
	assert.Contains(t, body, "data-mcp-oauth")
	assert.Contains(t, body, "Connected by session:s1")
	assert.NotContains(t, body, "every project using", "a project-scope grant is not labelled the instance's")
	assert.NotContains(t, body, "scraper.local", "the page shows no host")

	inherited, _ := rig765(t, "", mcpauth.ModeOAuth)
	body = inherited.keys.do(t, http.MethodGet, "/projects/alpha/mcp-tools?server=scraper", owner754, nil).Body.String()
	assert.Contains(t, body, "every project using scraper shares it")

	static, _ := rig765(t, "alpha", "static")
	body = static.keys.do(t, http.MethodGet, "/projects/alpha/mcp-tools?server=scraper", owner754, nil).Body.String()
	assert.NotContains(t, body, "data-mcp-oauth", "a non-OAuth server has no sign-in row")
}

func TestProjectMCPOAuth765_DisconnectDeletesTheScopeTheWiringReads(t *testing.T) {
	for scope, want := range map[string]string{"alpha": "alpha/scraper", "": "/scraper"} {
		r, got := rig765(t, scope, mcpauth.ModeOAuth)
		res := r.post(t, owner754, "alpha", disconnect765("scraper"))
		assert.Contains(t, res.Header.Get("Location"), "done=disconnected", "scope %q", scope)
		assert.Equal(t, []string{want}, *got, "scope %q", scope)
	}
}

func TestProjectMCPOAuth765_RefusalsDeleteNothing(t *testing.T) {
	r, got := rig765(t, "alpha", mcpauth.ModeOAuth)
	for _, name := range []string{"../x", "unlisted", ""} {
		res := r.post(t, owner754, "alpha", disconnect765(name))
		assert.Contains(t, res.Header.Get("Location"), "done=disconnect-refused", name)
	}
	// A project member meets the page's gate.
	rec := r.keys.do(t, http.MethodPost, "/projects/alpha/mcp-tools", member754, disconnect765("scraper"))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	// A GET never disconnects.
	r.keys.do(t, http.MethodGet, "/projects/alpha/mcp-tools?server=scraper&action=disconnect", owner754, nil)
	assert.Empty(t, *got)

	// No role holds mcp_tool_grant without mcp_oauth_connect today, so the
	// second check is exercised directly: a caller without it gets 403.
	req := setupAuthRequest(http.MethodPost, "/projects/alpha/mcp-tools", auth.RoleUser)
	req.Form = disconnect765("scraper")
	req.PostForm = req.Form
	w := httptest.NewRecorder()
	r.srv.mcpToolsDisconnect(w, req, "alpha")
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Empty(t, *got)

	// No connector: refused, nothing resolved.
	bare := newToolsRig780(t, false)
	res := bare.post(t, owner754, "alpha", disconnect765("scraper"))
	assert.Contains(t, res.Header.Get("Location"), "done=disconnect-refused")
	body := bare.keys.do(t, http.MethodGet, "/projects/alpha/mcp-tools?server=scraper", owner754, nil).Body.String()
	assert.NotContains(t, body, "data-mcp-oauth")
}

// Review 8ff1 F3: a failed delete reports an error, never a success.
func TestProjectMCPOAuth765_AFailedDisconnectSaysSo(t *testing.T) {
	r := newToolsRig780(t, false)
	var got []string
	WithMCPOAuthAdmin(oauth765{scope: "alpha", mode: mcpauth.ModeOAuth, disconnected: &got, fail: errors.New("store down")})(r.srv)
	res := r.post(t, owner754, "alpha", disconnect765("scraper"))
	assert.Contains(t, res.Header.Get("Location"), "done=error")
	assert.Empty(t, got)
}
