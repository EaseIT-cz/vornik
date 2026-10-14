package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"vornik.io/vornik/internal/agenttools"
	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/untrusted"
)

// ForgeIssueResolver retains all installation credentials on the daemon.
type ForgeIssueResolver interface {
	ForgeProvider(context.Context, string) (forge.ForgeProvider, error)
}

// WithForgeIssueResolver wires the project-bound provider resolver.
func WithForgeIssueResolver(r ForgeIssueResolver) ServerOption {
	return func(s *Server) { s.forgeIssueResolver = r }
}

type forgeIssueRequest struct {
	Operation   string            `json:"operation"`
	Repo        string            `json:"repo"`
	Number      int               `json:"number"`
	Filter      forge.IssueFilter `json:"filter"`
	Labels      []string          `json:"labels"`
	DuplicateOf int               `json:"duplicate_of"`
	Explanation string            `json:"explanation"`
}

var forgeIssueOperations = map[string]bool{"list_issues": false, "get_issue": false, "list_labels": false, "add_labels": true, "set_labels": true, "close_issue": true}

type forgeIssueAudit struct {
	Operation       string   `json:"operation"`
	Repo            string   `json:"repo"`
	Number          int      `json:"number,omitempty"`
	Labels          []string `json:"labels,omitempty"`
	DuplicateOf     int      `json:"duplicate_of,omitempty"`
	ExplanationHash string   `json:"explanation_hash,omitempty"`
	Bytes           int      `json:"bytes,omitempty"`
}

// AgentForgeIssues is task-only and fails closed on every role-resolution gap.
func (s *Server) AgentForgeIssues(w http.ResponseWriter, r *http.Request) {
	project, actx, req, ok := s.forgeIssuePreflight(w, r)
	if !ok {
		return
	}
	// One task's forge calls run one at a time, so the one-repo check below
	// and its close see every earlier call's audit row (review 59d1 F3).
	lock := s.forgeIssueTaskLock(actx.taskID)
	lock.Lock()
	defer lock.Unlock()
	isWrite := forgeIssueOperations[req.Operation]
	tool := "forge_" + req.Operation
	audit := forgeIssueAudit{Operation: req.Operation, Repo: req.Repo, Number: req.Number, Labels: req.Labels, DuplicateOf: req.DuplicateOf}
	if req.Explanation != "" {
		audit.ExplanationHash = fmt.Sprintf("%x", sha256.Sum256([]byte(req.Explanation)))
	}
	refuse := func(reason string) {
		_ = s.logForgeIssueAudit(r.Context(), project, actx, tool, audit, "refused")
		respondJSON(w, 200, AgentQueryResponse{Refusal: reason})
	}
	p, reason := s.forgeIssuePolicy(project, req, isWrite)
	if reason == "" && req.Operation == "close_issue" {
		reason = s.forgeIssueCrossRepoRefusal(r.Context(), actx.taskID, req.Repo)
	}
	if reason != "" {
		refuse(reason)
		return
	}
	if why, ok := s.forgeIssueBudget.reserveCall(actx.taskID, func() (apiTaskSpend, error) { return s.deriveForgeIssueSpend(r.Context(), actx.taskID) }); !ok {
		refuse(why)
		return
	}
	provider, err := s.forgeIssueResolver.ForgeProvider(r.Context(), project)
	if err != nil {
		refuse("Forge provider unavailable")
		return
	}
	reader, ok := provider.(forge.IssueReader)
	if !ok {
		refuse("Forge issue reads unsupported by provider")
		return
	}
	if isWrite {
		if err = s.logForgeIssueAudit(r.Context(), project, actx, tool, audit, "pending"); err != nil {
			respondJSON(w, 200, AgentQueryResponse{Refusal: "Forge write blocked: durable audit unavailable"})
			return
		}
	}
	result, err := applyForgeIssue(r.Context(), provider, reader, p.Forge.Issues, req, s.forgePublicationNotice())
	if err != nil {
		_ = s.logForgeIssueAudit(r.Context(), project, actx, tool, audit, "error")
		respondJSON(w, 200, AgentQueryResponse{Refusal: "Forge issue operation failed; check daemon audit and App permissions"})
		return
	}
	s.respondForgeIssueApplied(w, r, project, actx, tool, audit, isWrite, result)
}

// respondForgeIssueApplied audits and returns a call the provider completed:
// for a write, GitHub holds the change. No path here may audit it "refused"
// or leave it only "pending" (audit 2026-10-12, applied-write audit).
func (s *Server) respondForgeIssueApplied(w http.ResponseWriter, r *http.Request, project string, actx agentAPIContext, tool string, audit forgeIssueAudit, isWrite bool, result any) {
	marshal := json.Marshal
	if s.forgeIssueMarshalForTest != nil {
		marshal = s.forgeIssueMarshalForTest
	}
	raw, err := marshal(result)
	if err != nil {
		// Deliberately a call with zero bytes: nothing is returned, and the
		// row's zero byte figure matches the in-memory tracker.
		if aerr := s.logForgeIssueAudit(r.Context(), project, actx, tool, audit, forgeIssueStatusAppliedUnencoded); aerr != nil {
			s.logForgeIssueResultAuditFailure(actx, audit, isWrite, aerr)
		}
		respondJSON(w, 200, AgentQueryResponse{Refusal: forgeIssuePostCallReply(isWrite, "its result could not be encoded", "forge_result_unencoded")})
		return
	}
	safe := s.redactAgentAPIBody(string(raw), outputguard.ProvenanceThirdParty)
	capped, trunc := capToolResultBytes(safe, s.toolResultMaxBytes())
	body := untrusted.WrapLabeled("forge_issues", capped)
	audit.Bytes = len(body)
	s.forgeIssueBudget.addBytes(actx.taskID, int64(len(body)))
	if err = s.logForgeIssueAudit(r.Context(), project, actx, tool, audit, "ok"); err != nil {
		s.logForgeIssueResultAuditFailure(actx, audit, isWrite, err)
		respondJSON(w, 200, AgentQueryResponse{Refusal: forgeIssuePostCallReply(isWrite, "the daemon could not record its result audit", "forge_result_audit_failed")})
		return
	}
	respondJSON(w, 200, AgentQueryResponse{Body: body, Provenance: "third_party", Bytes: len(body), Truncated: trunc})
}

// forgeIssueStatusAppliedUnencoded audits a call the provider completed whose
// result could not be encoded for the agent: the call happened (outcome ok).
const forgeIssueStatusAppliedUnencoded = "applied_unencoded"

// forgeIssuePostCallReply is the reply for a call the provider completed
// whose result cannot be returned. A write says applied and do not repeat,
// so the agent does not re-send a non-idempotent close; a read may be
// retried. The class token is stable for callers that key off it.
func forgeIssuePostCallReply(isWrite bool, why, class string) string {
	if isWrite {
		return "Forge write applied, but " + why + "; do not repeat it (" + class + ")"
	}
	return "Forge read completed, but " + why + "; the result is withheld and the read may be retried (" + class + ")"
}

// logForgeIssueResultAuditFailure is the reconciliation record when the
// result row of a completed call cannot be written: for a write the pending
// row is then the only durable row, so the error log names the GitHub object
// it changed; for a read (no pending row) the log is the only trace.
func (s *Server) logForgeIssueResultAuditFailure(a agentAPIContext, p forgeIssueAudit, isWrite bool, err error) {
	s.logger.Error().Err(err).
		Str("task_id", a.taskID).Str("execution_id", a.executionID).
		Str("operation", p.Operation).Str("repo", p.Repo).Int("issue", p.Number).
		Strs("labels", p.Labels).Int("duplicate_of", p.DuplicateOf).Bool("write", isWrite).
		Msg("forge issue: operation applied on the forge but its result audit failed; reconcile the pending audit row against this object")
}
func (s *Server) logForgeIssueAudit(ctx context.Context, project string, a agentAPIContext, tool string, p forgeIssueAudit, status string) error {
	if s.toolAuditRepo == nil {
		return fmt.Errorf("audit unavailable")
	}
	input, _ := json.Marshal(p)
	out, _ := json.Marshal(map[string]string{"status": status, "role": a.role})
	outcome := ""
	if status == "ok" || status == forgeIssueStatusAppliedUnencoded {
		outcome = "ok"
	}
	if status == "error" || status == "refused" {
		outcome = "error"
	}
	return s.toolAuditRepo.Log(ctx, &persistence.ToolAuditEntry{ProjectID: project, TaskID: a.taskID, ExecutionID: a.executionID, ToolName: tool, ToolInput: string(input), ToolOutput: string(out), Outcome: outcome, CreatedAt: time.Now()})
}

// deriveForgeIssueSpend rebuilds a task's forge budget from its audit rows.
// It reads each forge tool name separately, so other tools' rows of the same
// task cannot crowd forge rows off the page (audit 2026-10-12: one unfiltered
// page undercounted). A full page means the count cannot be established from
// one page; the budget is then treated as exhausted (fail closed). Only the
// daemon's own rows count (a top-level JSON status other than pending); the
// entrypoint's per-call row is not a second call. Each counted row comes with
// at most one pending and one entrypoint row, so a full page of legitimate
// rows already holds more than maxAgentAPICallsPerTask counted rows; rows
// injected through the tool-audit route can only raise the count.
func (s *Server) deriveForgeIssueSpend(ctx context.Context, task string) (apiTaskSpend, error) {
	rows, complete, err := s.taskToolAuditRows(ctx, task, forgeIssueToolNames, forgeCrossRepoPageSize)
	if err != nil {
		return apiTaskSpend{}, err
	}
	if !complete {
		// Exactly exhausted: forgeIssueBudget is an apiBudgetTracker, whose
		// reserveCall enforces these same two ceilings.
		return apiTaskSpend{calls: maxAgentAPICallsPerTask, bytes: maxAgentAPIBytesPerTask}, nil
	}
	var out apiTaskSpend
	for _, row := range rows {
		var o struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(row.ToolOutput), &o) != nil || o.Status == "" || o.Status == "pending" {
			continue
		}
		out.calls++
		var p forgeIssueAudit
		if json.Unmarshal([]byte(row.ToolInput), &p) == nil && p.Bytes > 0 {
			out.bytes += int64(p.Bytes)
		}
	}
	return out, nil
}

func (s *Server) forgeIssuePreflight(w http.ResponseWriter, r *http.Request) (string, agentAPIContext, forgeIssueRequest, bool) {
	if r.Method != http.MethodPost {
		respondError(w, 405, "METHOD_NOT_ALLOWED", "only POST supported")
		return "", agentAPIContext{}, forgeIssueRequest{}, false
	}
	project := extractProjectID(r)
	if project == "" || !requestAllowsProject(r, project) {
		respondError(w, 403, "FORBIDDEN", "project not allowed")
		return "", agentAPIContext{}, forgeIssueRequest{}, false
	}
	actx, why, ok := s.resolveAgentAPIContext(r)
	if !ok {
		respondError(w, 403, "FORBIDDEN", why)
		return "", agentAPIContext{}, forgeIssueRequest{}, false
	}
	// Existing API binding tolerates missing execution for older tools; this
	// privileged surface requires a current task/project/execution binding.
	if actx.executionID == "" || s.executionRepo == nil || s.taskRepo == nil || s.projectRegistry == nil {
		respondError(w, 403, "FORBIDDEN", "current task execution required")
		return "", agentAPIContext{}, forgeIssueRequest{}, false
	}
	task, err := s.taskRepo.Get(r.Context(), actx.taskID)
	if err != nil || task == nil || task.ProjectID != project || task.Status != persistence.TaskStatusRunning {
		respondError(w, 403, "FORBIDDEN", "task is not running in this project")
		return "", agentAPIContext{}, forgeIssueRequest{}, false
	}
	exec, err := s.executionRepo.GetByTaskID(r.Context(), actx.taskID)
	if err != nil || exec == nil || exec.ID != actx.executionID || exec.ProjectID != project || exec.TaskID != actx.taskID {
		respondError(w, 403, "FORBIDDEN", "execution is not current in this project")
		return "", agentAPIContext{}, forgeIssueRequest{}, false
	}
	var req forgeIssueRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&req); err != nil {
		respondError(w, 400, "VALIDATION_ERROR", "invalid bounded Forge request")
		return "", agentAPIContext{}, forgeIssueRequest{}, false
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		respondError(w, 400, "VALIDATION_ERROR", "trailing Forge request data")
		return "", agentAPIContext{}, forgeIssueRequest{}, false
	}
	_, known := forgeIssueOperations[req.Operation]
	if !known {
		respondError(w, 400, "VALIDATION_ERROR", "unknown Forge issue operation")
		return "", agentAPIContext{}, forgeIssueRequest{}, false
	}
	tool := "forge_" + req.Operation
	resolve := s.roleToolAllowlistReason
	if s.agentRoleAllowlistForTest != nil {
		resolve = s.agentRoleAllowlistForTest
	}
	allowed, _ := resolve(r.Context(), actx.taskID)
	if len(allowed) == 0 || !agenttools.RoleAllowsTool(allowed, tool) {
		respondError(w, 403, "FORBIDDEN", "Forge tool is not in the calling role allowlist")
		return "", agentAPIContext{}, forgeIssueRequest{}, false
	}
	if why, agent := s.agentToolRefusal(r.Context(), project, actx.taskID, tool); agent && why != "" {
		respondError(w, 403, "FORBIDDEN", why)
		return "", agentAPIContext{}, forgeIssueRequest{}, false
	}
	return project, actx, req, true
}

func (s *Server) forgeIssuePolicy(project string, req forgeIssueRequest, isWrite bool) (*registry.Project, string) {
	p := s.projectRegistry.GetProject(project)
	if p == nil || !p.ForgeIssueRepoAllowed(req.Repo) {
		return nil, "Forge repo is outside project configured repos"
	}
	if _, configured := p.ResolveForge(); !configured || s.forgeIssueResolver == nil {
		return nil, "Forge provider not configured"
	}
	if req.Operation == "get_issue" || isWrite {
		if req.Number < 1 {
			return nil, "issue number must be positive"
		}
	}
	// Writes are decided from untrusted issue text and have no human
	// approval yet (audit 2026-10-12, finding C; design open question
	// Q-FI1). They need the operator's explicit acceptance.
	// see LLD § https://docs.vornik.io "Issue triage writes: prompt injection and approval (2026-10-12)"
	if isWrite && !p.Forge.Issues.WritesWithoutApproval {
		return nil, "Forge issue writes have no human approval yet; the operator must set forge.issues.writes_without_approval: true to accept writes steered by untrusted issue text"
	}
	if req.Operation == "list_issues" {
		if _, err := req.Filter.Resolve(); err != nil {
			return nil, err.Error()
		}
	}
	if req.Operation == "add_labels" || req.Operation == "set_labels" {
		if !p.Forge.Issues.LabelsAllowed(req.Labels) {
			return nil, "label writes disabled or requested label outside allowlist"
		}
	}
	if req.Operation == "close_issue" {
		if !p.Forge.Issues.CloseEnabled {
			return nil, "duplicate closing is disabled for this project"
		}
		if req.DuplicateOf < 1 || req.DuplicateOf == req.Number || strings.TrimSpace(req.Explanation) == "" || len(req.Explanation) > forge.MaxIssueTextBytes || !utf8.ValidString(req.Explanation) {
			return nil, "invalid duplicate reference or explanation"
		}
		if outputguard.ScanWithProvenance(req.Explanation, outputguard.ProvenanceThirdParty).HasFinding() {
			return nil, "duplicate explanation failed output safety scan"
		}
	}
	return p, ""
}

// forgeIssueLockStripes bounds the per-task lock memory. Unrelated tasks that
// share a stripe wait for each other; forge issue calls are rare and short.
const forgeIssueLockStripes = 64

// forgeIssueTaskLock is the stripe that serialises one task's forge calls.
func (s *Server) forgeIssueTaskLock(taskID string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(taskID))
	return &s.forgeIssueTaskLocks[h.Sum32()%forgeIssueLockStripes]
}

// forgeCrossRepoPageSize bounds each per-tool audit read of the one-repo
// check. One forge call writes at most three rows under its tool name (the
// daemon's pending and result rows, and the entrypoint's tool-call row), and
// the per-task budget allows maxAgentAPICallsPerTask calls, so a page this
// size holds every legitimate row; a full page means rows were injected and
// the check fails closed (review 59d1 F7).
const forgeCrossRepoPageSize = 3*maxAgentAPICallsPerTask + 16

// forgeIssueToolNames are the audit identities of every forge issue tool; the
// daemon's own rows and the entrypoint's tool-call rows both use them.
var forgeIssueToolNames = []string{"forge_list_issues", "forge_get_issue", "forge_list_labels", "forge_add_labels", "forge_set_labels", "forge_close_issue"}

// forgeIssueCrossRepoRefusal keeps a close inside one repo for the whole
// task: a close posts agent-written text publicly, and reads can span private
// allowlisted repos (audit 2026-10-12, finding C). Any forge tool-audit row of
// this task that names another repo, or no repo, refuses the close, whatever
// its status.
// Fails closed on a missing store, a query error, a full page, or a row whose
// input does not decode.
func (s *Server) forgeIssueCrossRepoRefusal(ctx context.Context, taskID, repo string) string {
	const unchecked = "duplicate close refused: this task's forge history could not be checked for another repo"
	if s.toolAuditRepo == nil || taskID == "" {
		return unchecked
	}
	rows, complete, err := s.taskToolAuditRows(ctx, taskID, forgeIssueToolNames, forgeCrossRepoPageSize)
	if err != nil || !complete {
		return unchecked
	}
	for _, row := range rows {
		var in struct {
			Repo string `json:"repo"`
		}
		if json.Unmarshal([]byte(row.ToolInput), &in) != nil {
			return unchecked
		}
		// An absent or empty repo refuses too (review 59d1 F2).
		if !strings.EqualFold(in.Repo, repo) {
			return "duplicate close refused: this task has used the forge tools on another repo; close duplicates in a task that touches only " + repo
		}
	}
	return ""
}

// forgePublicationNotice is the Art 50 notice a forge comment the daemon posts
// for an agent carries (GitHub #342). Unwired → the zero Notice, which the
// provider resolves to the compiled default: still disclosed, never silent.
func (s *Server) forgePublicationNotice() aidisclosure.Notice {
	if s == nil || s.aiDisclosure == nil {
		return aidisclosure.Notice{}
	}
	return s.aiDisclosure.PublicationNotice()
}

func applyForgeIssue(ctx context.Context, provider forge.ForgeProvider, reader forge.IssueReader, policy registry.ProjectForgeIssues, req forgeIssueRequest, notice aidisclosure.Notice) (any, error) {
	var result any
	var err error
	switch req.Operation {
	case "list_issues":
		result, err = reader.ListIssues(ctx, req.Repo, req.Filter)
	case "get_issue":
		result, err = reader.GetIssue(ctx, req.Repo, req.Number)
	case "list_labels":
		result, err = reader.ListLabels(ctx, req.Repo)
	default:
		writer, supported := provider.(forge.IssueWriter)
		if !supported {
			err = fmt.Errorf("issue writes unsupported")
			break
		}
		switch req.Operation {
		case "add_labels":
			err = writer.AddLabels(ctx, req.Repo, req.Number, req.Labels)
		case "set_labels":
			var issue forge.Issue
			issue, err = reader.GetIssue(ctx, req.Repo, req.Number)
			if err == nil {
				err = writer.SetLabels(ctx, req.Repo, req.Number, policy.ManagedLabels(issue.Labels, req.Labels))
			}
		case "close_issue":
			err = writer.CloseIssue(ctx, req.Repo, req.Number, forge.CloseIssueSpec{DuplicateOf: req.DuplicateOf, Explanation: req.Explanation, Notice: notice})
		}
		result = map[string]bool{"applied": err == nil}
	}
	return result, err
}
