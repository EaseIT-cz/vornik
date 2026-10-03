package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentadmin"
)

// /ui/admin/agents and /ui/admin/agents/<ns> (agent-administered Vornik
// design §10.3, plan P6.5): what each connected assistant has set up, in a
// person's words. Read-only; behind the admin session like every
// /ui/admin page, so an agent admin key (a companion key, never a session)
// cannot reach it.

// AgentRow is one connected namespace.
type AgentRow struct {
	Namespace  string
	ClientKind string
	Class      string // mcp_only | shell_capable
	KeyPrefix  string
	KeyStatus  string // active | none
	LastUsed   string // "" when never
	Projects   int
	Pending    int
}

// AgentItem is one workflow, integration or credential with its status.
type AgentItem struct {
	Name   string
	Status string
	Detail string
}

// AgentProject is one project of a namespace.
type AgentProject struct {
	ID           string
	Purpose      string
	BudgetUSD    float64
	Workflows    []AgentItem
	Integrations []AgentItem
	Credentials  []AgentItem
}

// AgentRequest is one approval request.
type AgentRequest struct {
	Sentence string
	When     string
}

// AgentPage is one namespace in full.
type AgentPage struct {
	Row        AgentRow
	Claims     []string
	BudgetUSD  float64
	CeilingUSD float64
	Projects   []AgentProject
	Pending    []AgentRequest
	Failed     []AgentRequest
	Expired    []AgentRequest
	// Models are the namespace's model destination approvals
	// ("<sub-provider>@<host>", status approved or withdrawn; design §18.6
	// item 2). Only this console withdraws one.
	Models []AgentItem
}

// AgentsSource is the service behind the pages.
type AgentsSource interface {
	ListAgents(ctx context.Context) ([]AgentRow, error)
	// DescribeAgent returns nil for a namespace that does not exist.
	DescribeAgent(ctx context.Context, ns string) (*AgentPage, error)
}

// ModelWithdrawer withdraws a namespace's model destination approval (sets
// removed_at; agent-administered design §18.6 item 2, round 2 F7, round 3
// F3). The operator console is its only caller; a source test pins that.
type ModelWithdrawer interface {
	// WithdrawModelDestination reports whether a live approval was
	// withdrawn (false: absent or already withdrawn).
	WithdrawModelDestination(ctx context.Context, ns, destination string) (bool, error)
}

// WithAgents wires the agents pages.
func WithAgents(src AgentsSource) ServerOption {
	return func(s *Server) { s.agents = src }
}

// AdminAgentsData backs admin_agents.html.
type AdminAgentsData struct {
	adminCommonData
	Available bool
	Rows      []AgentRow
	Page      *AgentPage
	Error     string
}

// AdminAgents renders /ui/admin/agents and /ui/admin/agents/<ns>.
func (s *Server) AdminAgents(w http.ResponseWriter, r *http.Request, ns string) {
	data := AdminAgentsData{
		adminCommonData: adminCommonData{Title: "Assistants", CurrentPage: "admin-agents", IsAdmin: true},
		Available:       s.agents != nil,
	}
	if !data.Available {
		s.render(w, "admin_agents.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ns = strings.Trim(ns, "/")
	if ns == "" {
		rows, err := s.agents.ListAgents(ctx)
		if err != nil {
			// The detail goes to the log, not the page (review 20261002-d930 F5).
			s.logger.Warn().Err(err).Msg("admin agents: list failed")
			data.Error = "Some assistants could not be read; the daemon log has the detail."
			if errors.Is(err, agentadmin.ErrUnavailable) {
				data.Error = err.Error()
			}
		}
		data.Rows = rows
		s.render(w, "admin_agents.html", data)
		return
	}
	page, err := s.agents.DescribeAgent(ctx, ns)
	switch {
	case err != nil:
		s.logger.Warn().Err(err).Str("namespace", ns).Msg("admin agents: describe failed")
		data.Error = "This assistant's setup could not be read; the daemon log has the detail."
		if errors.Is(err, agentadmin.ErrUnavailable) {
			data.Error = err.Error()
		}
	case page == nil:
		http.NotFound(w, r)
		return
	}
	data.Page = page
	if page != nil {
		data.Title = "Assistant " + page.Row.Namespace
	}
	s.render(w, "admin_agents.html", data)
}

// AdminAgentWithdrawModel handles POST /ui/admin/agents/<ns>/models/withdraw:
// the operator withdraws an approved model destination. A role still on a
// model that goes there is refused before its next attempt, and the
// assistant's next define_swarm for it asks the phone again.
func (s *Server) AdminAgentWithdrawModel(w http.ResponseWriter, r *http.Request, ns string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	wd, ok := s.agents.(ModelWithdrawer)
	if s.agents == nil || !ok {
		http.Error(w, "agent administration is not wired", http.StatusServiceUnavailable)
		return
	}
	dest := strings.TrimSpace(r.FormValue("destination"))
	if dest == "" || !strings.Contains(dest, "@") {
		http.Error(w, "destination required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	removed, err := wd.WithdrawModelDestination(ctx, ns, dest)
	if err != nil {
		s.logger.Warn().Err(err).Str("namespace", ns).Str("destination", dest).Msg("admin agents: withdraw model destination failed")
		http.Error(w, "the approval could not be withdrawn; the daemon log has the detail", http.StatusInternalServerError)
		return
	}
	if !removed {
		// Absent or already withdrawn (review 20261003-a525 A4): say so.
		http.Error(w, "nothing to withdraw: "+dest+" is not an approved model destination of "+ns, http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/ui/admin/agents/"+ns, http.StatusSeeOther)
}
