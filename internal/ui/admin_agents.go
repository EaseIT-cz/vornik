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
}

// AgentsSource is the service behind the pages.
type AgentsSource interface {
	ListAgents(ctx context.Context) ([]AgentRow, error)
	// DescribeAgent returns nil for a namespace that does not exist.
	DescribeAgent(ctx context.Context, ns string) (*AgentPage, error)
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
