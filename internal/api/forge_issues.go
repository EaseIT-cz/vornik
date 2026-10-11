package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
	"vornik.io/vornik/internal/agenttools"
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
	result, err := applyForgeIssue(r.Context(), provider, reader, p.Forge.Issues, req)
	status := "ok"
	if err != nil {
		status = "error"
	}
	if err != nil {
		_ = s.logForgeIssueAudit(r.Context(), project, actx, tool, audit, status)
		respondJSON(w, 200, AgentQueryResponse{Refusal: "Forge issue operation failed; check daemon audit and App permissions"})
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		refuse("Forge result encoding failed")
		return
	}
	safe := s.redactAgentAPIBody(string(raw), outputguard.ProvenanceThirdParty)
	capped, trunc := capToolResultBytes(safe, s.toolResultMaxBytes())
	body := untrusted.WrapLabeled("forge_issues", capped)
	audit.Bytes = len(body)
	if err = s.logForgeIssueAudit(r.Context(), project, actx, tool, audit, status); err != nil {
		respondJSON(w, 200, AgentQueryResponse{Refusal: "Forge result uncertain: pending attempt audited but result audit failed"})
		return
	}
	s.forgeIssueBudget.addBytes(actx.taskID, int64(len(body)))
	respondJSON(w, 200, AgentQueryResponse{Body: body, Provenance: "third_party", Bytes: len(body), Truncated: trunc})
}
func (s *Server) logForgeIssueAudit(ctx context.Context, project string, a agentAPIContext, tool string, p forgeIssueAudit, status string) error {
	if s.toolAuditRepo == nil {
		return fmt.Errorf("audit unavailable")
	}
	input, _ := json.Marshal(p)
	out, _ := json.Marshal(map[string]string{"status": status, "role": a.role})
	outcome := ""
	if status == "ok" {
		outcome = "ok"
	}
	if status == "error" || status == "refused" {
		outcome = "error"
	}
	return s.toolAuditRepo.Log(ctx, &persistence.ToolAuditEntry{ProjectID: project, TaskID: a.taskID, ExecutionID: a.executionID, ToolName: tool, ToolInput: string(input), ToolOutput: string(out), Outcome: outcome, CreatedAt: time.Now()})
}
func (s *Server) deriveForgeIssueSpend(ctx context.Context, task string) (apiTaskSpend, error) {
	if s.toolAuditRepo == nil {
		return apiTaskSpend{}, fmt.Errorf("audit unavailable")
	}
	rows, err := s.toolAuditRepo.List(ctx, persistence.ToolAuditFilter{TaskID: &task, PageSize: agentAPIBudgetSeedPageSize})
	if err != nil {
		return apiTaskSpend{}, err
	}
	var out apiTaskSpend
	for _, row := range rows {
		if row == nil || !strings.HasPrefix(row.ToolName, "forge_") {
			continue
		}
		if strings.Contains(row.ToolOutput, `"status":"pending"`) {
			continue
		}
		out.calls++
		var p forgeIssueAudit
		if json.Unmarshal([]byte(row.ToolInput), &p) == nil {
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

func applyForgeIssue(ctx context.Context, provider forge.ForgeProvider, reader forge.IssueReader, policy registry.ProjectForgeIssues, req forgeIssueRequest) (any, error) {
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
			err = writer.CloseIssue(ctx, req.Repo, req.Number, forge.CloseIssueSpec{DuplicateOf: req.DuplicateOf, Explanation: req.Explanation})
		}
		result = map[string]bool{"applied": err == nil}
	}
	return result, err
}
