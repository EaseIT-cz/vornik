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
}

// Grant is written only by the widening_change effect.
type Grant struct {
	Integrations []IntegrationGrant `json:"integrations,omitempty"`
	// Workflows maps a workflow ID to its approved reach-signature hash.
	Workflows map[string]string `json:"workflows,omitempty"`
	// CeilingUSD, when set, is the namespace ceiling the approval raises to.
	CeilingUSD *float64 `json:"ceiling_usd,omitempty"`
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
}

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
