package persistence

import (
	"context"
	"time"
)

// The agent approval tables (agent-administered Vornik design §7.3, §7.2,
// §7.6; plan P3.4). They hold what a person approved on a device: an
// integration's approved tools, a workflow's approved reach, a namespace's
// budget ceiling. Grants are written only by the widening_change effect;
// the inert path may only withdraw them (Mark*Removed, DeleteWorkflowReach).
// Every row carries its namespace, so a namespace's rows are read by an
// exact match, never a LIKE pattern.

// AgentIntegrationApproval is one approved integration of one project.
type AgentIntegrationApproval struct {
	Namespace        string
	ProjectID        string
	Integration      string
	Kind             string // mcp | api
	URL              string
	ReadTools        []string
	WriteTools       []string
	ReadPending      bool
	ApprovedByDevice string
	ApprovedAt       time.Time
	RemovedAt        *time.Time
}

// AgentWorkflowApproval is one workflow's approved reach signature.
type AgentWorkflowApproval struct {
	Namespace        string
	WorkflowID       string
	ProjectID        string
	ReachHash        string
	ApprovedByDevice string
	ApprovedAt       time.Time
}

// AgentNamespaceBudget is a namespace's approved budget ceiling.
type AgentNamespaceBudget struct {
	Namespace        string
	CeilingUSD       float64
	ApprovedByDevice string
	UpdatedAt        time.Time
}

// AgentModelDestinationApproval is one model destination a person approved
// for a namespace (agent-administered design §18.6 item 2 in detail): a role
// of the namespace may run on a model the live router sends there. The
// destination is "<sub-provider>@<endpoint host>". Two writers only (round 3
// F3): the widening_change effect inserts (or re-grants), the operator
// console sets removed_at; a source test pins both.
type AgentModelDestinationApproval struct {
	Namespace        string
	Destination      string
	ApprovedByDevice string
	ApprovedAt       time.Time
	RemovedAt        *time.Time
}

// AgentGrantRepository persists the approval tables.
type AgentGrantRepository interface {
	// UpsertIntegration grants (or re-grants, clearing removed_at).
	UpsertIntegration(ctx context.Context, a AgentIntegrationApproval) error
	// GetIntegration returns ErrNotFound when there is no row (removed rows
	// are returned; the caller checks RemovedAt).
	GetIntegration(ctx context.Context, projectID, integration string) (*AgentIntegrationApproval, error)
	ListIntegrations(ctx context.Context, namespace string) ([]AgentIntegrationApproval, error)
	// MarkIntegrationRemoved withdraws an approval, keeping its history.
	MarkIntegrationRemoved(ctx context.Context, projectID, integration string, at time.Time) error

	UpsertWorkflowReach(ctx context.Context, a AgentWorkflowApproval) error
	// GetWorkflowReach returns ErrNotFound when the workflow has none.
	GetWorkflowReach(ctx context.Context, workflowID string) (*AgentWorkflowApproval, error)
	ListWorkflowReach(ctx context.Context, namespace string) ([]AgentWorkflowApproval, error)
	DeleteWorkflowReach(ctx context.Context, workflowID string) error

	UpsertCeiling(ctx context.Context, b AgentNamespaceBudget) error
	// GetCeiling returns ErrNotFound until a raise was approved.
	GetCeiling(ctx context.Context, namespace string) (*AgentNamespaceBudget, error)

	// UpsertModelDestination grants a model destination (or re-grants one
	// the console removed, clearing removed_at).
	UpsertModelDestination(ctx context.Context, a AgentModelDestinationApproval) error
	// GetModelDestination returns ErrNotFound when there is no row (removed
	// rows are returned; the caller checks RemovedAt).
	GetModelDestination(ctx context.Context, namespace, destination string) (*AgentModelDestinationApproval, error)
	// ListModelDestinations returns a namespace's rows, removed ones too.
	ListModelDestinations(ctx context.Context, namespace string) ([]AgentModelDestinationApproval, error)
	// MarkModelDestinationRemoved withdraws an approval, keeping its row; it
	// reports whether a live row was withdrawn (false: absent or already
	// removed).
	MarkModelDestinationRemoved(ctx context.Context, namespace, destination string, at time.Time) (bool, error)
}
