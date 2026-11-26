package api

// Agent-administered design part 24 §24.3 B2-B3 (GitHub #920): the daemon
// side of hand_over. It shares FireScheduledBroker's checks (the namespace's
// live admin key, brokerDelegable, input validation with the document
// bounds, the key's companion budget, createBrokerTask's setup gate) and
// differs in four ways: the target needs no schedule; the inputs are
// hand_over.inputs plus each mapped document read from the store; the
// creation source is HANDOVER; and the idempotency key is the source task
// plus the target workflow.

import (
	"context"
	"encoding/json"
	"fmt"
	"path"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskcreate"
)

// handOverPage is the page size of the source execution's artifact scan.
const handOverPage = 500

func refuseHandOver(reason, detail string) error {
	return &executor.HandOverRefusal{Reason: reason, Detail: detail}
}

// FireHandover implements executor.HandOverFirer.
func (s *Server) FireHandover(ctx context.Context, source *persistence.Task, execution *persistence.Execution, wf *registry.Workflow) (string, error) {
	h := wf.Broker.HandOver
	ns, ok := agentns.FromID(wf.ID)
	if !ok {
		return "", refuseHandOver("not_delegable", "the source is not an agent workflow")
	}
	depth := handOverDepth(source.Payload) + 1
	if depth > registry.HandOverMaxDepth {
		return "", refuseHandOver("depth", fmt.Sprintf("this would be hand-over %d of a chain; at most %d follow one delegation", depth, registry.HandOverMaxDepth))
	}
	key, err := s.liveAgentAdminKey(ctx, ns)
	if err != nil {
		return "", err
	}
	judge := key
	if judge == nil {
		judge = &persistence.APIKey{AgentAdmin: true, AgentNamespace: ns, ProjectID: agentns.ID(ns, "home")}
	}
	target, project, err := s.brokerDelegable(judge, delegateArgs{Workflow: h.Workflow})
	if err != nil {
		return "", refuseHandOver("not_delegable", err.Error())
	}
	inputs := map[string]any{}
	for k, v := range h.Inputs {
		inputs[k] = v
	}
	for _, prop := range h.DocumentProperties() {
		content, found, err := s.handOverDocument(ctx, source, execution, h.Documents[prop])
		if err != nil {
			return "", err
		}
		if !found {
			return "", refuseHandOver("missing_document", fmt.Sprintf("the job finished without %s", h.Documents[prop]))
		}
		inputs[prop] = content
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		return "", err
	}
	validated, err := validateBrokerInputs(target, raw)
	if err != nil {
		return "", refuseHandOver("inputs", err.Error())
	}
	if key != nil {
		if err := s.checkCompanionKeyBudget(ctx, key); err != nil {
			return "", refuseHandOver("budget", err.Error())
		}
	}
	task, err := s.createBrokerTask(ctx, brokerTaskParams{wf: target, project: project, inputs: validated, key: key,
		taskType: target.ID, source: persistence.TaskCreationSourceHandover,
		idemKey: "handover:" + source.ID + ":" + target.ID,
		extra: map[string]any{
			"handed_over_from": map[string]any{"project": source.ProjectID, "workflow": wf.ID, "task": source.ID},
			"handover_depth":   depth,
		}})
	if err != nil {
		if ce := taskcreate.AsError(err); ce != nil && ce.Reason == taskcreate.ReasonSetupIncomplete {
			return "", refuseHandOver("setup", ce.Message)
		}
		return "", err
	}
	return task.ID, nil
}

// handOverDepth is a task's position in a hand-over chain: 0 unless a
// hand-over started it. createBrokerTask writes extra fields into the task's
// context, so that is the one place it is read (review-20261125-23bb F3).
func handOverDepth(payload []byte) int {
	var p struct {
		Context struct {
			Depth int `json:"handover_depth"`
		} `json:"context"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return 0
	}
	return p.Context.Depth
}

// handOverDocument reads the source execution's harvested output named by
// rel (artifacts/out/<name>) from the store, never from a container path.
// found is false when the job produced no such file. The document bound is
// enforced by validateBrokerInputs; this read stops one byte past the
// largest allowed document so an oversized file is refused, not truncated.
func (s *Server) handOverDocument(ctx context.Context, source *persistence.Task, execution *persistence.Execution, rel string) (string, bool, error) {
	if s.artifactRepo == nil {
		return "", false, fmt.Errorf("the artifact repository is not wired")
	}
	execID := execution.ID
	name := path.Base(rel)
	var row *persistence.Artifact
	// Every page: a long execution can hold more rows than one page
	// (review-20261125-23bb F2).
	for offset := 0; ; offset += handOverPage {
		rows, err := s.artifactRepo.List(ctx, persistence.ArtifactFilter{ExecutionID: &execID, PageSize: handOverPage, Offset: offset})
		if err != nil {
			return "", false, fmt.Errorf("list the job's outputs: %w", err)
		}
		for _, a := range rows {
			// The harvest stores <stem>-<date>-<exec suffix><ext>; compare the
			// name the step wrote, as the broker artifact egress does (#926).
			if a == nil || a.ProjectID != source.ProjectID || a.ArtifactClass != persistence.ArtifactClassOutput || executor.OriginalArtifactName(a.Name) != name {
				continue
			}
			if row == nil || a.CreatedAt.After(row.CreatedAt) {
				row = a // the latest version of the name
			}
		}
		if len(rows) < handOverPage {
			break
		}
	}
	if row == nil {
		return "", false, nil
	}
	b, _, err := s.readArtifactBytes(ctx, row, registry.BrokerDocumentMaxBytes+1)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", name, err)
	}
	return string(b), true, nil
}
