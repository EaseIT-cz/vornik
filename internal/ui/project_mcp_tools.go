package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// The project page that grants an MCP server's tools to the project's roles
// and applies it in one step (2026-07-08-control-plane-hub-design.md,
// amendment 2026-11-15, GitHub #780). It reuses #627's grid, validator,
// planner and proposal; its button also approves the proposal as the
// clicking person and applies it through the engine, so the managed
// instance's owner, who holds neither the admin hub nor proposal approval,
// can put a server to use.

// mcpToolsSource marks this page's proposals in their Evidence (O4).
const mcpToolsSource = "mcp-tools"

// mcpToolsPending is an approved proposal of this page still to apply.
type mcpToolsPending struct{ ID, Title string }

// mcpToolsMay admits an admin or a caller the mcp_tool_grant surface allows
// (the owner); a project-scoped member gets 404, anyone else 403 (O2).
func (s *Server) mcpToolsMay(w http.ResponseWriter, r *http.Request) bool {
	if admin.IsAdminFromContext(r.Context()) {
		return true
	}
	switch api.OperatorSurfaceAccessFor(r, s.isAdminKeyForSurface, api.SurfaceMCPToolGrant) {
	case api.OperatorSurfaceAllowed:
		return true
	case api.OperatorSurfaceHidden:
		http.NotFound(w, r)
	default:
		http.Error(w, "admin scope required", http.StatusForbidden)
	}
	return false
}

// ProjectMCPTools serves GET and POST /ui/projects/<id>/mcp-tools (O2 to O4).
func (s *Server) ProjectMCPTools(w http.ResponseWriter, r *http.Request, projectID string) {
	if !s.mcpToolsMay(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.render(w, "admin_control_plane_mcp_use.html", s.mcpToolsPage(r, projectID, strings.TrimSpace(r.URL.Query().Get("server"))))
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		_ = r.ParseForm()
		switch r.PostFormValue("action") {
		case "apply", "withdraw":
			s.mcpToolsPendingAction(w, r, projectID, r.PostFormValue("action"))
			return
		}
		s.mcpToolsGrant(w, r, projectID)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// mcpToolsPage is the grid for one project: a server picker, the chosen
// server's tools and this project's roles, its own grants only, and its
// pending applies.
func (s *Server) mcpToolsPage(r *http.Request, projectID, server string) MCPUseData {
	data := MCPUseData{Title: "MCP tools for " + projectID, CurrentPage: "projects", Project: projectID, ProjectMode: true,
		Flash: mcpToolsFlash[r.URL.Query().Get("done")]}
	if s.mcpRegistrySource != nil {
		servers, err := s.mcpRegistrySource.Servers(r.Context())
		if err != nil {
			s.logger.Warn().Err(err).Msg("mcp tools page: the MCP registry could not be read")
			data.Error = "The MCP server list could not be read. Try again in a moment."
		}
		for _, sv := range servers {
			data.Servers = append(data.Servers, sv.Name)
		}
	}
	if server != "" && mcpServerNameRe.MatchString(server) {
		built := s.mcpUseBuild(r.Context(), server, projectID)
		built.ProjectMode, built.Servers, built.Flash, built.Title, built.CurrentPage = true, data.Servers, data.Flash, data.Title, data.CurrentPage
		var own []mcpUseGrantRow
		for _, g := range built.Grants {
			if g.Project == projectID {
				own = append(own, g)
			}
		}
		built.Grants = own
		data = built
	}
	data.Pending = s.mcpToolsPendingFor(r.Context(), projectID)
	return data
}

// mcpToolsFlash maps the page's outcome tokens to sentences.
var mcpToolsFlash = map[string]string{
	"applied":            "Applied: the roles' tool lists are updated.",
	"busy":               "Approved. It applies when no task of this project is running: press Apply now then.",
	"ack-required":       "This swarm also serves other projects: tick the box that says so, then apply.",
	"stale-base":         "A file changed since the page was opened. Reload and try again.",
	mcpUseUnchanged:      "Nothing to change.",
	mcpUseRefused:        "That change could not be made: a tool or role the page did not offer, or a file that would not load.",
	"not-this-page":      "That proposal is not one of this page's for this project.",
	"withdrawn":          "Withdrawn: the change will not be applied.",
	"apply-failed":       "The change was rejected and rolled back. The operator can see why on the Proposals tab.",
	"error":              "Something went wrong. Try again in a moment.",
	"not-approved":       "That proposal is not approved.",
	"wall-clock-lowered": "The change was refused by its re-validation.",
}

// mcpToolsPendingFor lists this project's approved, unapplied proposals of
// this page (O4).
func (s *Server) mcpToolsPendingFor(ctx context.Context, projectID string) []mcpToolsPending {
	if s.proposalStore == nil {
		return nil
	}
	ps, err := s.proposalStore.List(ctx, persistence.ProposalListFilter{ProjectID: projectID, Statuses: []string{persistence.ProposalStatusApproved}})
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).Msg("mcp tools page: pending proposals could not be listed")
		return nil
	}
	var out []mcpToolsPending
	for _, p := range ps {
		if mcpToolsProposal(p, projectID) {
			out = append(out, mcpToolsPending{ID: p.ID, Title: p.Title})
		}
	}
	return out
}

// mcpToolsEvidence is the part of a proposal's Evidence this page reads.
type mcpToolsEvidence struct {
	Source    string `json:"source"`
	AckShared bool   `json:"ack_shared"`
}

// mcpToolsProposal reports whether p is this page's proposal for projectID.
func mcpToolsProposal(p *persistence.ControlPlaneProposal, projectID string) bool {
	var ev mcpToolsEvidence
	return p != nil && p.ProjectID == projectID && json.Unmarshal([]byte(p.Evidence), &ev) == nil && ev.Source == mcpToolsSource
}

func (s *Server) mcpToolsRedirect(w http.ResponseWriter, r *http.Request, projectID, server, token string) {
	q := url.Values{"done": {token}}
	if server != "" {
		q.Set("server", server)
	}
	http.Redirect(w, r, "/ui/projects/"+url.PathEscape(projectID)+"/mcp-tools?"+q.Encode(), http.StatusSeeOther)
}

// mcpToolsGrant files #627's proposal, approves it as the actor and applies
// it (O3).
func (s *Server) mcpToolsGrant(w http.ResponseWriter, r *http.Request, projectID string) {
	server := strings.TrimSpace(r.PostFormValue("server"))
	filed := s.mcpUseFileFromForm(r, server, projectID, mcpToolsSource, true)
	token := filed.token
	if token == "" {
		token = s.mcpToolsApprove(r, filed.id)
	}
	s.mcpToolsAudit(r, filed.id, map[string]any{"project": projectID, "server": server, "tools": filed.granted, "outcome": token})
	s.mcpToolsRedirect(w, r, projectID, server, token)
}

// mcpToolsApprove approves a filed proposal as the actor and applies it.
func (s *Server) mcpToolsApprove(r *http.Request, id string) string {
	if s.proposalApplier == nil {
		return "error"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	actor := keysActor(r)
	if actor == "" {
		// The ledger's approver is this page's only record of who decided
		// (SetStatus's self-approval check cannot fire: the proposer is
		// the reserved operator-ui), so it is never empty (review ba11 F2).
		return "error"
	}
	if err := s.proposalStore.SetStatus(ctx, id, persistence.ProposalStatusApproved, actor); err != nil {
		return "error"
	}
	return s.mcpToolsApply(ctx, id, actor)
}

// mcpToolsApply applies an approved proposal of this page. A shared swarm's
// acknowledgement was required before it was filed (O3), and is recorded in
// its Evidence.
func (s *Server) mcpToolsApply(ctx context.Context, id, actor string) string {
	p, err := s.proposalStore.GetByID(ctx, id)
	if err != nil {
		return "error"
	}
	var ev mcpToolsEvidence
	_ = json.Unmarshal([]byte(p.Evidence), &ev)
	return cpApplyOutcome(s.proposalApplier.Apply(ctx, id, actor, ev.AckShared))
}

// mcpToolsPendingAction applies or withdraws one of this page's pending
// proposals (O4). Any other proposal, another project's, another source's or
// one with no source, is refused. A shared swarm's proposal was filed only
// after its acknowledgement (O3), which its Evidence records and the apply
// passes on, so Apply now cannot skip it.
func (s *Server) mcpToolsPendingAction(w http.ResponseWriter, r *http.Request, projectID, action string) {
	id := strings.TrimSpace(r.PostFormValue("id"))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	token := "not-this-page"
	actor := keysActor(r)
	if actor == "" {
		token = "error" // the ledger records who decided (review 27ed F5)
	} else if s.proposalStore != nil && s.proposalApplier != nil {
		if p, err := s.proposalStore.GetByID(ctx, id); err == nil && mcpToolsProposal(p, projectID) && p.Status == persistence.ProposalStatusApproved {
			switch action {
			case "apply":
				token = s.mcpToolsApply(ctx, id, actor)
			default:
				token = "withdrawn"
				if err := s.proposalStore.SetStatus(ctx, id, persistence.ProposalStatusRejected, actor); err != nil {
					token = "error"
				}
			}
		}
	}
	s.mcpToolsAudit(r, id, map[string]any{"project": projectID, "action": action, "outcome": token})
	s.mcpToolsRedirect(w, r, projectID, "", token)
}

// mcpToolsAudit writes the page's mcp.tools.grant row (O6).
func (s *Server) mcpToolsAudit(r *http.Request, target string, detail map[string]any) {
	if s.adminAuditRepo == nil {
		return
	}
	entry := &persistence.AdminAuditEntry{ID: persistence.GenerateID("admaud"), Principal: keysActor(r), Source: "ui",
		Action: "mcp.tools.grant", Target: target, IP: clientIP(r), UserAgent: r.UserAgent()}
	if b, err := json.Marshal(detail); err == nil {
		entry.After = string(b)
	}
	if err := s.adminAuditRepo.Insert(r.Context(), entry); err != nil {
		s.logger.Warn().Err(err).Str("target", target).Msg("mcp tools page: audit write failed")
	}
}
