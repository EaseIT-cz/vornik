package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"vornik.io/vornik/internal/egressscan"
	"vornik.io/vornik/internal/secrets"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/brokeractions"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Per-action approval of an agent project's proposed writes on the approver
// device (agent-administered Vornik plan P4.8). The broker write-actions
// machinery is reused unchanged: staging, promotion, the action row and its
// guarded transitions, the worker and its re-check. What is new is the
// surface: one approval request per pending action, whose approve effect is
// the same pending->approved transition /inbox makes, then the worker's
// kick; and whose reject is /inbox's reject.

// actionPayload is a broker_action request's typed content: the action and
// exactly what it will send, shown behind the page's disclosure.
type actionPayload struct {
	ActionID   string          `json:"action_id"`
	Project    string          `json:"project"`
	Workflow   string          `json:"workflow"`
	Action     string          `json:"action"`
	Tool       string          `json:"tool"`
	Args       json.RawMessage `json:"args"`
	ArgsSHA256 string          `json:"args_sha256"`
}

func actionRequestID(actionID string) string { return "apr_ba_" + actionID }

// fileActionApprovals files a phone approval for each pending action of an
// agent project's task. Idempotent: the request ID is derived from the
// action, so a second call files nothing new.
func (s *agentAdminService) fileActionApprovals(ctx context.Context, projectID, taskID string) {
	ns, agent := agentns.FromID(projectID)
	if !agent || s.c.repos == nil || s.c.repos.BrokerActions == nil {
		return
	}
	rows, err := s.c.repos.BrokerActions.ListByTask(ctx, taskID)
	if err != nil {
		s.c.Logger.Error().Err(err).Str("task_id", taskID).Msg("agent actions: could not list the task's actions")
		return
	}
	for _, a := range rows {
		if a == nil || a.Status != persistence.BrokerActionPending || a.ProjectID != projectID {
			continue
		}
		if err := s.fileActionApproval(ctx, ns, a); err != nil {
			s.c.Logger.Error().Err(err).Str("action_id", a.ActionID).Msg("agent actions: could not file the phone approval")
		}
	}
}

func (s *agentAdminService) fileActionApproval(ctx context.Context, ns string, a *persistence.BrokerAction) error {
	id := actionRequestID(a.ActionID)
	if _, err := s.requests.GetRequest(ctx, id); err == nil {
		return nil // already filed
	}
	raw, err := json.Marshal(actionPayload{ActionID: a.ActionID, Project: a.ProjectID, Workflow: a.WorkflowID,
		Action: a.ActionKind, Tool: a.Tool, Args: json.RawMessage(a.ArgsJSON), ArgsSHA256: a.ArgsSHA256})
	if err != nil {
		return err
	}
	canon, err := approval.Canonical(raw)
	if err != nil {
		return err
	}
	sum, err := approval.CanonicalSHA256(canon)
	if err != nil {
		return err
	}
	// The sentence names the action and where it goes, never its arguments:
	// the push carries the sentence, and the arguments (a message body, an
	// amount) are for the device's page only.
	sentence := fmt.Sprintf("Your assistant (%s) wants to %s with %s. Review exactly what it will send before you approve.",
		ns, strings.ReplaceAll(a.ActionKind, "_", " "), describeWrite(a.Tool))
	err = s.devices.FileRequest(ctx, persistence.AgentApprovalRequestRow{
		ID: id, Namespace: ns, Kind: persistence.ApprovalKindBrokerAction, Sentence: sentence,
		Rendered: canon, RenderedSHA256: sum, Status: persistence.ApprovalPending,
		CreatedAt: time.Now().UTC(), ExpiresAt: a.ExpiresAt,
	})
	if err != nil {
		if _, gerr := s.requests.GetRequest(ctx, id); gerr == nil {
			return nil // a concurrent filing won (review 20261002-52aa #1)
		}
	}
	return err
}

// describeWrite names a proposable write for a person.
func describeWrite(tool string) string {
	if api, method, path, ok := (registry.BrokerProposal{Tool: tool}).APITool(); ok {
		return "the API " + api + " (" + method + " " + path + ")"
	}
	if server, name, ok := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__"); ok {
		return agentns.IntegrationOf(server) + " (" + name + ")"
	}
	return tool
}

// actionEffect approves the action exactly as /inbox does - a guarded
// pending->approved transition bound to the shown arguments' hash - then
// kicks the worker. Never a bare kick.
func (s *agentAdminService) actionEffect(ctx context.Context, r persistence.AgentApprovalRequestRow) error {
	var pl actionPayload
	if json.Unmarshal(r.Rendered, &pl) != nil || pl.ActionID == "" {
		return fmt.Errorf("%w: the request does not name its action", approverdevice.ErrPermanent)
	}
	repo := s.c.repos.BrokerActions
	err := repo.Approve(ctx, pl.ActionID, pl.ArgsSHA256, "device:"+r.DecidedByDevice, time.Now().UTC())
	if errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		a, gerr := repo.Get(ctx, pl.ActionID)
		if gerr != nil {
			return gerr
		}
		switch a.Status {
		case persistence.BrokerActionApproved, persistence.BrokerActionExecuting, persistence.BrokerActionExecuted:
			// A re-applied effect: already approved. The kick below is
			// intentional - after a crash it resumes an approved action
			// the worker never picked up; the worker's lease makes a
			// second kick harmless (review 20261002-52aa #3).
		default:
			return fmt.Errorf("%w: the action is %s", approverdevice.ErrPermanent, a.Status)
		}
	} else if err != nil {
		return err
	}
	s.c.brokerActionWorker.Kick(pl.ActionID)
	s.c.companionPusher.Kick()
	return nil
}

// actionRejected is /inbox's reject.
func (s *agentAdminService) actionRejected(ctx context.Context, r persistence.AgentApprovalRequestRow) {
	var pl actionPayload
	if json.Unmarshal(r.Rendered, &pl) != nil || pl.ActionID == "" {
		return
	}
	if err := s.c.repos.BrokerActions.Reject(ctx, pl.ActionID, "device:"+r.DecidedByDevice, time.Now().UTC()); err != nil &&
		!errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		// The person rejected it; the action cannot execute (it is not
		// approved), but it stays pending until it expires (review
		// 20261002-52aa #2). Error level, so an operator sees it.
		s.c.Logger.Error().Err(err).Str("action_id", pl.ActionID).Msg("agent actions: a rejected action could not be marked rejected; it stays pending until it expires")
	}
	s.c.companionPusher.Kick()
}

// actionCaller routes the worker's calls: an agent API write to the
// project's agent API client on its write route (exactly the action's
// method), anything else to the MCP manager.
type actionCaller struct {
	c   *Container
	mcp brokeractions.Caller
}

func (a actionCaller) CallToolOnce(ctx context.Context, projectID, tool, argsJSON string) (string, bool, error) {
	// The arguments were scanned at staging; scanned again at the seam that
	// sends them, so the cover does not rest on the staged body being the
	// sent one (review 20261002-3ef1 R3). Agent projects only; fail closed.
	if _, agent := agentns.FromID(projectID); agent {
		if why := a.c.agentArgsAtSend(projectID, tool, argsJSON); why != "" {
			return "", false, fmt.Errorf("%w: %s", mcp.ErrNotSent, why)
		}
	}
	api, method, path, ok := (registry.BrokerProposal{Tool: tool}).APITool()
	if !ok {
		return a.mcp.CallToolOnce(ctx, projectID, tool, argsJSON)
	}
	cl := a.c.agentAPIClient(projectID, true, map[string]bool{method: true})
	if cl == nil {
		return "", false, fmt.Errorf("%w: the agent API client is not available", mcp.ErrNotSent)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &body); err != nil {
		return "", false, fmt.Errorf("%w: the arguments are not a JSON object", mcp.ErrNotSent)
	}
	res, err := cl.Call(ctx, apigateway.Request{Provider: api, Method: method, Path: path, Body: body})
	if err != nil {
		if errors.Is(err, apigateway.ErrGatewayRequest) {
			return "", false, err // may have been sent: unknown
		}
		return "", false, fmt.Errorf("%w: %v", mcp.ErrNotSent, err)
	}
	return res.Body, res.Status >= 400, nil
}

// agentArgsAtSend scans an agent action's arguments as they are sent.
func (c *Container) agentArgsAtSend(projectID, tool, argsJSON string) string {
	scan := c.egressScanner()
	if scan == nil || scan.Detector == nil {
		return "the egress secret scan is not available"
	}
	fs, err := egressscan.ScanJSON(scan.Detector, []byte(argsJSON))
	if err != nil {
		return "the egress secret scan failed"
	}
	c.recordEgress(egressscan.SurfaceActionArgs, projectID, tool, fs, secrets.ActionBlock)
	if f, credential := egressscan.Blocking(fs); credential {
		return "the arguments carry " + f.String()
	}
	return ""
}
