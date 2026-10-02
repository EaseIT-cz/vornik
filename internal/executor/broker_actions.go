package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/egressscan"
	"vornik.io/vornik/internal/secrets"

	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Broker write actions — https://docs.vornik.io
// 2026-09-29-broker-write-actions-and-push-design.md §5.1.
//
// A workflow that declares broker.proposes writes each proposal to a .json
// output file. Before the task's COMPLETED transition the executor STAGES a
// broker_actions row per declared action: invisible and unapprovable. Right
// after the transition it PROMOTES the task's staged rows to pending. Every
// COMPLETED transition in this package calls both (a source scan pins it).

// WithBrokerActions wires the broker-action store. nil leaves staging off.
func WithBrokerActions(repo persistence.BrokerActionRepository) Option {
	return func(e *Executor) { e.brokerActions = repo }
}

// BrokerActionNotifyFunc alerts operators that n writes from taskID wait in
// /inbox. It must carry no arguments (design §5.3: notify-only).
type BrokerActionNotifyFunc func(ctx context.Context, projectID, taskID string, n int)

// WithBrokerActionNotifier wires the pending-approval alert. Optional.
func WithBrokerActionNotifier(fn BrokerActionNotifyFunc) Option {
	return func(e *Executor) { e.brokerActionNotify = fn }
}

// brokerActionStoreError is a failure to persist a proposal. It is
// retryable: the task takes the ordinary failure path and re-runs.
type brokerActionStoreError struct{ err error }

func (e *brokerActionStoreError) Error() string        { return "broker_action_store: " + e.err.Error() }
func (e *brokerActionStoreError) Unwrap() error        { return e.err }
func (e *brokerActionStoreError) FailureClass() string { return "broker_action_store" }

// stageBrokerActions stages the task's proposals. It returns an error only
// when a row could not be written or a persisted proposal could not be read;
// a missing or invalid proposal is an outcome, stored as a terminal row.
func (e *Executor) stageBrokerActions(ctx context.Context, task *persistence.Task, execution *persistence.Execution) error {
	if e.brokerActions == nil || e.workflows == nil || execution == nil {
		return nil
	}
	wf := e.workflows.GetWorkflow(execution.WorkflowID)
	if wf == nil || wf.Broker == nil || len(wf.Broker.Proposes) == 0 {
		return nil
	}
	arts, err := e.taskOutputArtifacts(ctx, task.ID)
	if err != nil {
		return &brokerActionStoreError{err: fmt.Errorf("list artifacts: %w", err)}
	}
	now := time.Now().UTC()
	for _, prop := range wf.Broker.Proposes {
		row, err := e.brokerActionRow(ctx, task, wf, prop, arts, now)
		if err != nil {
			return &brokerActionStoreError{err: err}
		}
		if _, err := e.brokerActions.Stage(ctx, row); err != nil {
			return &brokerActionStoreError{err: fmt.Errorf("stage %s: %w", prop.Action, err)}
		}
	}
	return nil
}

// promoteBrokerActions moves the task's staged rows to pending. A failure
// is logged, not fatal: the startup sweep promotes staged rows of COMPLETED
// tasks.
func (e *Executor) promoteBrokerActions(ctx context.Context, task *persistence.Task) {
	if e.brokerActions == nil {
		return
	}
	if n, err := e.brokerActions.PromoteStaged(ctx, task.ID); err != nil {
		e.logger.Warn().Err(err).Str("task_id", task.ID).
			Msg("broker actions: promotion failed; the startup sweep will promote them")
	} else if n > 0 {
		e.logger.Info().Str("task_id", task.ID).Int64("actions", n).
			Msg("broker actions: proposals awaiting approval in /inbox")
		if e.brokerActionNotify != nil {
			e.brokerActionNotify(ctx, task.ProjectID, task.ID, int(n))
		}
	}
}

func (e *Executor) taskOutputArtifacts(ctx context.Context, taskID string) ([]*persistence.Artifact, error) {
	if e.artifactRepo == nil {
		return nil, nil
	}
	id := taskID
	return e.artifactRepo.List(ctx, persistence.ArtifactFilter{TaskID: &id, PageSize: 500})
}

// brokerActionRow builds the row for one declared action: staged with
// canonical args when the proposal is valid, otherwise a terminal
// proposal_missing / proposal_invalid row with empty args.
func (e *Executor) brokerActionRow(ctx context.Context, task *persistence.Task, wf *registry.Workflow,
	prop registry.BrokerProposal, arts []*persistence.Artifact, now time.Time,
) (*persistence.BrokerAction, error) {
	ttl, _ := prop.EffectiveApprovalTTL() // validated at load
	row := &persistence.BrokerAction{
		ProjectID: task.ProjectID, TaskID: task.ID, WorkflowID: wf.ID,
		ActionKind: prop.Action, Tool: prop.Tool,
		ArgsJSON: []byte(`{}`), CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if task.CreatedByAPIKeyID != nil {
		row.APIKeyID = *task.CreatedByAPIKeyID
	}
	emptyHash, _ := approval.CanonicalSHA256(row.ArgsJSON)
	row.ArgsSHA256 = emptyHash

	art := newestOutputNamed(arts, prop.Output)
	if art == nil {
		row.Status = persistence.BrokerActionProposalMissing
		return row, nil
	}
	body, err := e.readArtifactCapped(ctx, art, prop.EffectiveMaxArgsBytes())
	var args []byte
	reason := ""
	switch {
	case errors.Is(err, errProposalOversize):
		reason = "oversize"
	case err != nil:
		return nil, fmt.Errorf("read proposal %s: %w", art.Name, err)
	default:
		args, reason = parseBrokerProposal(body, prop)
	}
	if reason == "" {
		reason = e.agentArgsRefusal(task.ProjectID, prop.Action, args)
	}
	if reason != "" {
		e.logger.Warn().Str("task_id", task.ID).Str("action", prop.Action).Str("reason", reason).
			Msg("broker actions: proposal invalid")
		row.Status = persistence.BrokerActionProposalInvalid
		return row, nil
	}
	hash, _ := approval.CanonicalSHA256(args)
	row.ArgsJSON, row.ArgsSHA256, row.Status = args, hash, persistence.BrokerActionStaged
	return row, nil
}

// parseBrokerProposal validates a proposal file and returns its canonical
// args, or a reason it is invalid. The reason is for the operator log only.
func parseBrokerProposal(body []byte, prop registry.BrokerProposal) ([]byte, string) {
	if len(body) > prop.EffectiveMaxArgsBytes() {
		return nil, "oversize"
	}
	var doc struct {
		Action string          `json:"action"`
		Args   json.RawMessage `json:"args"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, "not a {action, args} JSON document"
	}
	if doc.Action != prop.Action {
		return nil, "names an undeclared action"
	}
	canon, err := approval.Canonical(doc.Args)
	if err != nil {
		return nil, "args are not canonicalisable JSON (duplicate keys?)"
	}
	schema, err := registry.CompileBrokerSchema(prop.Action+"-args", prop.ArgsSchema)
	if err != nil {
		return nil, "args_schema does not compile"
	}
	var v any
	vdec := json.NewDecoder(bytes.NewReader(canon))
	vdec.UseNumber()
	if err := vdec.Decode(&v); err != nil || schema.Validate(v) != nil {
		return nil, "args fail args_schema"
	}
	return canon, ""
}

func newestOutputNamed(arts []*persistence.Artifact, name string) *persistence.Artifact {
	var pick *persistence.Artifact
	for _, a := range arts {
		if a == nil || a.ArtifactClass != persistence.ArtifactClassOutput || OriginalArtifactName(a.Name) != name {
			continue
		}
		if pick == nil || a.CreatedAt.After(pick.CreatedAt) {
			pick = a
		}
	}
	return pick
}

// errProposalOversize: the proposal file is larger than max_args_bytes.
var errProposalOversize = errors.New("proposal larger than max_args_bytes")

// readArtifactCapped reads an artifact of at most limit bytes, through the
// artifact store when one is wired, else from its storage path. A recorded
// size above the limit is refused without reading the file.
func (e *Executor) readArtifactCapped(ctx context.Context, a *persistence.Artifact, limit int) ([]byte, error) {
	if a.SizeBytes != nil && *a.SizeBytes > int64(limit) {
		return nil, errProposalOversize
	}
	var body []byte
	var err error
	if e.artifactStore != nil {
		body, err = e.artifactStore.Retrieve(ctx, a.ID)
	} else {
		body, err = os.ReadFile(a.StoragePath)
	}
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, errProposalOversize
	}
	return body, nil
}

// brokerDeclaredOutputs returns the output file names a broker workflow
// declares (egress and every proposal), which memory ingest skips.
func (e *Executor) brokerDeclaredOutputs(workflowID string) map[string]bool {
	if e.workflows == nil || workflowID == "" {
		return nil
	}
	wf := e.workflows.GetWorkflow(workflowID)
	if wf == nil || wf.Broker == nil {
		return nil
	}
	out := map[string]bool{}
	if wf.Broker.Egress.Output != "" {
		out[wf.Broker.Egress.Output] = true
	}
	for _, p := range wf.Broker.Proposes {
		out[p.Output] = true
	}
	return out
}

// agentArgsRefusal scans an agent project's proposed arguments (plan P5.3):
// a credential-shaped value makes the proposal invalid, so a person is never
// asked to approve sending a key. It fails closed. Operator projects pass.
func (e *Executor) agentArgsRefusal(projectID, action string, args []byte) string {
	if _, agent := agentns.FromID(projectID); !agent {
		return ""
	}
	if e.egressDetector == nil {
		return "the egress secret scan is not available"
	}
	fs, err := egressscan.ScanJSON(e.egressDetector, args)
	if err != nil {
		return "the egress secret scan failed"
	}
	if e.egressRecord != nil {
		e.egressRecord(egressscan.SurfaceActionArgs, projectID, action, fs, secrets.ActionBlock)
	}
	if f, credential := egressscan.Blocking(fs); credential {
		return "args carry " + f.String()
	}
	return ""
}
