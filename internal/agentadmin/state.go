package agentadmin

import (
	"sort"

	"vornik.io/vornik/internal/registry"
)

// State is the namespace's current, approved state, read by the caller from
// the live registry, the config tree and the approval tables. Every map is
// keyed by full ID ("<ns>--<slug>").
type State struct {
	Namespace string
	// DefaultBudgetUSD is a new project's monthly cap; CeilingUSD the
	// namespace's approved ceiling (the configured default until a device
	// approves a raise).
	DefaultBudgetUSD float64
	CeilingUSD       float64
	// AgentImage is the runtime image agent roles run in.
	AgentImage string
	// WritesOn is the daemon's broker.writes: proposals need it (P4.8).
	WritesOn bool

	Projects  map[string]*ProjectState
	Workflows map[string]*WorkflowState
	// FileHashes maps a configs-relative path of this namespace's files to
	// its current sha256 (hex). A path absent from it does not exist.
	FileHashes map[string]string
	// Locked holds every path and entity ID a pending change holds.
	Locked map[string]bool
	// Approvals maps project ID → integration name → its approval.
	Approvals map[string]map[string]IntegrationApproval
	// Advertised maps an MCP server URL to the tools it advertised when the
	// caller listed them for this verb call; absent means "could not list".
	Advertised map[string][]string
}

// ProjectState is one agent project as loaded.
type ProjectState struct {
	ID                string
	DisplayName       string
	Purpose           string
	MonthlyUSD        float64
	DefaultWorkflowID string
	Home              bool
	Swarm             SwarmState
	Servers           []ServerState
	APIs              []APIState
	Secrets           []string
	// Loaded and LoadedSwarm are the registry objects, for reach signatures.
	Loaded      *registry.Project
	LoadedSwarm *registry.Swarm
}

// SwarmState is a project's swarm (one per project, same slug).
type SwarmState struct {
	ID    string
	Roles []RoleSpec
}

// RoleSpec is one role.
type RoleSpec struct {
	Name         string
	Description  string
	Instructions string
	Tools        []string
}

// ServerState is one project-scoped MCP server as rendered.
type ServerState struct {
	Name    string
	URL     string
	AuthRef string // "secret://<ns>/<NAME>" or ""
	// OAuth marks auth mode oauth; Scopes are what the grant asks for.
	OAuth  bool
	Scopes []string
	Write  bool
	Tools  []string
}

// WorkflowState is one agent workflow as loaded.
type WorkflowState struct {
	ID      string
	Project string
	// ApprovedReach is the approved reach-signature hash, or "" when none
	// was ever approved.
	ApprovedReach string
	// Loaded is the registry object, for reach signatures.
	Loaded *registry.Workflow
}

// IntegrationApproval is one row of agent_integration_approvals.
type IntegrationApproval struct {
	Kind        string
	URL         string
	Read        []string
	Write       []string
	ReadPending bool
	Removed     bool
}

// Live reports whether the approval is in force.
func (a IntegrationApproval) Live() bool { return !a.Removed }

// budgetTotal sums the namespace's project caps.
func (s *State) budgetTotal() float64 {
	t := 0.0
	for _, p := range s.Projects {
		t += p.MonthlyUSD
	}
	return t
}

// workflowsOf returns the IDs of a project's workflows, sorted.
func (s *State) workflowsOf(project string) []string {
	var out []string
	for id := range s.Workflows {
		if ProjectOfWorkflow(id) == project {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
