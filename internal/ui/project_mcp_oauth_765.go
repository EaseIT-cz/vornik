package ui

// The project MCP tools page's OAuth row (GitHub #765;
// 2026-07-08-control-plane-hub-design.md, Amendment 2026-11-18 (#765)): the
// managed instance's owner sees whether the project's wiring holds a grant
// for the chosen server, and disconnects it. The row and the POST need the
// mcp_oauth_connect surface on top of the page's mcp_tool_grant.

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/mcpauth"
)

// mcpToolsOAuth is the OAuth row for one server.
type mcpToolsOAuth struct {
	Connected      bool
	ConnectedBy    string
	ConnectedAt    string
	ExpiresAt      string
	NeedsReconnect bool
	// Inherited is true when the grant the project reads is the instance's
	// (daemon scope): a disconnect removes it for every project that
	// inherits it.
	Inherited bool
}

// mcpToolsMayConnect reports whether the caller holds mcp_oauth_connect.
func (s *Server) mcpToolsMayConnect(r *http.Request) bool {
	if admin.IsAdminFromContext(r.Context()) {
		return true
	}
	return api.OperatorSurfaceAccessFor(r, s.isAdminKeyForSurface, api.SurfaceMCPOAuthConnect) == api.OperatorSurfaceAllowed
}

// mcpToolsListed reports whether server is one the page offers.
func (s *Server) mcpToolsListed(ctx context.Context, server string) bool {
	if s.mcpRegistrySource == nil || !mcpServerNameRe.MatchString(server) {
		return false
	}
	servers, err := s.mcpRegistrySource.Servers(ctx)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(servers, func(sv MCPRegistryServer) bool { return sv.Name == server })
}

// mcpToolsOAuthRow is the row for an OAuth-mode server the project's wiring
// resolves, or nil.
func (s *Server) mcpToolsOAuthRow(ctx context.Context, projectID, server string) *mcpToolsOAuth {
	if s.mcpOAuthAdmin == nil || !s.mcpToolsListed(ctx, server) {
		return nil
	}
	ref, ok := s.mcpOAuthAdmin.ResolveServer(projectID, server)
	if !ok || ref.Auth.EffectiveMode() != mcpauth.ModeOAuth {
		return nil
	}
	row := &mcpToolsOAuth{Inherited: ref.ProjectID == ""}
	tok, err := s.mcpOAuthAdmin.Grant(ctx, ref.ProjectID, server)
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).Str("server", server).Msg("mcp tools page: the grant could not be read")
		return row
	}
	if tok == nil {
		return row
	}
	row.Connected, row.ConnectedBy, row.NeedsReconnect = true, tok.ConnectedBy, tok.NeedsReconnect
	row.ConnectedAt = tok.ConnectedAt.UTC().Format(time.RFC3339)
	if tok.ExpiresAt != nil {
		row.ExpiresAt = tok.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return row
}

// mcpToolsDisconnect deletes the grant the project's wiring reads for a
// listed, resolvable server: the same scope the API's disconnect uses.
func (s *Server) mcpToolsDisconnect(w http.ResponseWriter, r *http.Request, projectID string) {
	server := strings.TrimSpace(r.PostFormValue("server"))
	if !s.mcpToolsMayConnect(r) {
		http.Error(w, "the mcp_oauth_connect surface is required", http.StatusForbidden)
		return
	}
	actor := keysActor(r)
	if s.mcpOAuthAdmin == nil || actor == "" || !s.mcpToolsListed(r.Context(), server) {
		s.mcpToolsRedirect(w, r, projectID, server, "disconnect-refused")
		return
	}
	ref, ok := s.mcpOAuthAdmin.ResolveServer(projectID, server)
	if !ok {
		s.mcpToolsRedirect(w, r, projectID, server, "disconnect-refused")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.mcpOAuthAdmin.Disconnect(ctx, ref.ProjectID, server, actor); err != nil {
		s.logger.Error().Err(err).Str("project", projectID).Str("server", server).Msg("mcp tools page: disconnect failed")
		s.mcpToolsRedirect(w, r, projectID, server, "error")
		return
	}
	s.logger.Info().Str("surface", string(api.SurfaceMCPOAuthConnect)).Str("project", projectID).Str("server", server).
		Bool("inherited", ref.ProjectID == "").Str("actor", actor).Msg("mcp tools page: disconnected")
	s.mcpToolsRedirect(w, r, projectID, server, "disconnected")
}
