// Package agentadmin renders and classifies the agent admin verbs
// (agent-administered Vornik design §6–§7). An agent supplies typed fields
// only; this package turns them into config files from the agent templates
// (configs/agent-templates), decides whether the change is inert, widening or
// refused, and states the change in a sentence a person can approve.
//
// It is pure: no I/O, no clock, no registry access. The caller (the service)
// reads the namespace's State, files the Change through the proposal ledger,
// and applies it or asks a device to approve it.
package agentadmin

import (
	"errors"
	"fmt"
)

// Class is a change's reach class (§7.1). The most restrictive class of any
// element of a change wins.
type Class int

// Classes, least to most restrictive.
const (
	Inert Class = iota
	Widening
	Refused
)

func (c Class) String() string {
	switch c {
	case Inert:
		return "inert"
	case Widening:
		return "widening"
	default:
		return "refused"
	}
}

// FileOp is one config file operation, with a path relative to the configs
// directory ("projects/<id>.yaml", "swarms/<id>.md", "workflows/<id>.md").
type FileOp struct {
	Op      string `json:"op"` // create | replace | delete
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
}

// Op names, matching the apply engine's.
const (
	OpCreate  = "create"
	OpReplace = "replace"
	OpDelete  = "delete"
)

// ReadSetAbsent is the read-set value for "this path must not exist"; it
// equals persistence.JournalReadSetAbsent (pinned by a test).
const ReadSetAbsent = "ABSENT"

// Change is one rendered verb call.
type Change struct {
	Verb      string
	Namespace string
	Class     Class
	// Reason is the refusal, in plain words for the agent to relay (§12).
	Reason string
	Ops    []FileOp
	// ReadSet is every file the change was rendered against: an op's target
	// (its current hash, or ReadSetAbsent for a create) and every file it
	// read. The apply engine refuses the change if any of them moved.
	ReadSet map[string]string
	// Locks are the paths and entity IDs the change holds while pending
	// (plan amendment 3).
	Locks []string
	// Sentence is the approval sentence, rendered from typed fields only.
	Sentence string
	// Rendered is the canonical JSON of the typed change shown behind the
	// approval page's disclosure.
	Rendered []byte
	// Grant is what an approval of this change records in the approval
	// tables; Narrow is what applying it records even when inert (removals).
	Grant  Grant
	Narrow Narrowing
	// Slot, when set, makes the change a credential_slot request: no file
	// op, no proposal; the device page stores the value (§8.2).
	Slot *CredentialSlot
	// Plain is the plain-language summary and risk level shown first on the
	// approval page (design §18.7); set for every change that goes to the
	// phone, and part of Rendered.
	Plain *PlainView
	// MissingTools, on a refused tools approval of a recipe server, are the
	// recipe tools the server does not offer (design §19.8 F6): the typed
	// signal list_my_setup's note is built from (review 20261003-6b46 F2).
	MissingTools []string
	// Credentials are the credential requests a recipe install files after
	// its approval applies, one per credential (design §19.9): the ones
	// still missing then are requested on the phone.
	Credentials []CredentialFollowUp
	// reach is the workflow's reach signature, kept for the plain view.
	reach *ReachSignature
	// recipe is the recipe an install_recipe change installs, for the plain
	// view.
	recipe *Recipe
}

// CredentialFollowUp is one credential a recipe install asks for once its
// approval has applied (design §19.2, §19.11).
type CredentialFollowUp struct {
	Project string `json:"project"` // the full project ID
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Purpose string `json:"purpose"`
}

// Grant is written only by the widening_change effect.
type Grant struct {
	Integrations []IntegrationGrant `json:"integrations,omitempty"`
	// Workflows maps a workflow ID to its approved reach-signature hash.
	Workflows map[string]string `json:"workflows,omitempty"`
	// AddsUSD is what a spending change (a project creation, a budget raise)
	// adds to the namespace sum; MaxTotalUSD the highest namespace total its
	// sentence stated: the conditional figure when other spending requests
	// were waiting, else the primary one. Unset when the sentence stated no
	// total (a raise within the ceiling). At apply the daemon refuses the
	// change if the sum with it exceeds max(ceiling, MaxTotalUSD), and
	// otherwise sets the ceiling to max(ceiling, sum) (design §18.4).
	AddsUSD     *float64 `json:"adds_usd,omitempty"`
	MaxTotalUSD *float64 `json:"max_total_usd,omitempty"`
	// CeilingUSD is the absolute ceiling requests filed before §18.4 pinned
	// (incident 2026-10-02). It is never written now; a pending request that
	// carries it is read as MaxTotalUSD (CeilingAfter).
	CeilingUSD *float64 `json:"ceiling_usd,omitempty"`
	// Cover marks the daemon's own cover request (design §18.4 F10): a
	// namespace already above its ceiling (incident 2026-10-02, claudecode
	// $108 against $104) is asked to approve the sum it has. Its apply takes
	// the equality branch: the sum must still be exactly MaxTotalUSD. No
	// verb sets it; only the daemon's filing path does, and it is part of
	// the canonical document the device approves.
	Cover bool `json:"cover,omitempty"`
	// Models are the remote model destinations a define_swarm approval
	// records for the namespace (design §18.6 item 2): one per destination
	// the namespace had not approved.
	Models []ModelGrant `json:"models,omitempty"`
}

// IntegrationGrant is one approved integration of one project.
type IntegrationGrant struct {
	Project     string   `json:"project"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"` // mcp | api
	URL         string   `json:"url"`
	Read        []string `json:"read,omitempty"`
	Write       []string `json:"write,omitempty"`
	ReadPending bool     `json:"read_pending,omitempty"`
}

// Narrowing is what an inert removal records: approvals it withdraws.
type Narrowing struct {
	RemovedIntegrations []IntegrationRef `json:"removed_integrations,omitempty"`
	RemovedWorkflows    []string         `json:"removed_workflows,omitempty"`
	RemovedProjects     []string         `json:"removed_projects,omitempty"`
	// RemovedTools are read tools a recipe install drops from an approved
	// integration (design §19.9 F2): they leave its approval, applied
	// without one, recorded and counted (§19.11).
	RemovedTools []ToolRemoval `json:"removed_tools,omitempty"`
}

// ToolRemoval is read tools leaving one integration's approval.
type ToolRemoval struct {
	Project string   `json:"project"`
	Name    string   `json:"name"`
	Tools   []string `json:"tools"`
	// Kind is the narrowings counter's label (review 20261003-6b46 F4).
	Kind string `json:"kind"`
}

// NarrowingRecipeTools is the kind of a removal a recipe install makes.
const NarrowingRecipeTools = "recipe_tools"

// IntegrationRef names one integration of one project.
type IntegrationRef struct {
	Project string `json:"project"`
	Name    string `json:"name"`
}

// Result is a mutating verb's outcome as the agent sees it (§6).
type Result struct {
	ChangeID    string `json:"change_id,omitempty"`
	Effect      string `json:"effect"` // applied | awaiting_approval | refused
	ApprovalURL string `json:"approval_url,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Sentence    string `json:"sentence,omitempty"`
	// MissingTools is Change.MissingTools of a refusal, for the daemon's
	// own callers; never sent to the agent.
	MissingTools []string `json:"-"`
}

// Effects.
const (
	EffectApplied  = "applied"
	EffectAwaiting = "awaiting_approval"
	EffectRefused  = "refused"
)

// ErrUnavailable is returned while the daemon's agent admin service has not
// been built: the agent templates are not installed yet.
var ErrUnavailable = errors.New("agent administration is not available yet: the agent templates are not installed (make install-config-assets)")

// ErrUnknownVerb is returned for a verb this package does not render.
var ErrUnknownVerb = errors.New("agentadmin: unknown verb")

// refuse builds a refused change; the reason is shown to the agent verbatim.
func refuse(verb, ns, format string, a ...any) Change {
	return Change{Verb: verb, Namespace: ns, Class: Refused, Reason: verb + " refused: " + fmt.Sprintf(format, a...)}
}
