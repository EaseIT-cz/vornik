package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/observability/logring"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/postmortem"
	"vornik.io/vornik/internal/report"
	"vornik.io/vornik/internal/stepoutcome"
)

// Diagnosis tools for agentic clients (live-task-observation design, Change
// 2026-10-08 C6, C6b, C7; GitHub #92): daemon_log and doctor for agent-admin
// keys, task_explain for any companion key. All read daemon-held state; none
// runs a program, calls the doctor HTTP route, or repairs anything.

const (
	daemonLogDefaultLimit = 100
	daemonLogMaxLimit     = 200
	// daemonLogMaxTaskLookups caps the task-repository reads one call makes
	// to resolve project-less lines (security scan of f1eb16871).
	daemonLogMaxTaskLookups = 32
	// doctorFailedWindow is how far back the namespace report counts failures.
	doctorFailedWindow = 24 * time.Hour
	// doctorFailedScanLimit and doctorMaxProjects bound one doctor call's
	// failure scan; a hit sets failed_tasks_truncated (review-20261008-a4ef F3).
	doctorFailedScanLimit = 500
	doctorMaxProjects     = 64
	// doctorRateCost is how many of the shared per-key tokens one doctor call
	// takes: it does the most work of the four tools (review-20261008-a4ef F3).
	doctorRateCost = 5
)

// agentAdminOnlyTools are refused to any key that is not an agent-admin key
// (C6b): they read a namespace, which only such a key has.
var agentAdminOnlyTools = map[string]bool{"daemon_log": true, "doctor": true}

// errAgentAdminOnly is the refusal for agentAdminOnlyTools.
func errAgentAdminOnly(tool string) error {
	return fmt.Errorf("AGENT_ADMIN_ONLY: %s is available to an agent admin key only", tool)
}

func diagToolDefs() []mcpToolDef {
	return []mcpToolDef{
		{
			Name: "task_explain",
			Description: "Explain why a task failed: the task's status and failure class, each step's outcome and class, the failure-class playbook (cause and suggestions), and the last error, redacted for you. " +
				"For a broker workflow only the closed error class and the playbook id. No model is called.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"task_id": map[string]any{"type": "string"}},
				"required":   []string{"task_id"},
			},
		},
		{
			Name: "daemon_log",
			Description: "Agent admin keys only: recent daemon log events about your namespace's projects (task lifecycle, MCP connections), " +
				"newest last: time, level, component, ids, server, a closed code such as auth_rejected for a refused MCP connection, " +
				"and the message only when it is one of Vornik's own fixed messages (no free text). At most the last hour.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"project": map[string]any{"type": "string"},
					"task_id": map[string]any{"type": "string"},
					"since":   map[string]any{"type": "string", "description": "RFC 3339; clamped to the oldest line held"},
					"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": daemonLogMaxLimit},
				},
			},
		},
		{
			Name: "doctor",
			Description: "Agent admin keys only: your namespace's health. Each project's MCP servers that are not connected, " +
				"with a closed reason code, and the tasks that failed in the last 24 hours by class. Read-only.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// ---- daemon_log ---------------------------------------------------------

type daemonLogArgs struct {
	Project string `json:"project"`
	TaskID  string `json:"task_id"`
	Since   string `json:"since"`
	Limit   int    `json:"limit"`
}

func (s *Server) companionToolDaemonLog(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	args, err := s.daemonLogQuery(ctx, key, raw)
	if err != nil {
		return "", err
	}
	out, withheld, skipped := s.filterDaemonLines(ctx, key, args)
	if withheld > 0 {
		recordLogWithheld("daemon_log", withheld)
	}
	recordLogToolOutcome("daemon_log", "ok")
	resp := map[string]any{"lines": out}
	if skipped > 0 {
		resp["unresolved_skipped"] = skipped
	}
	if oldest := s.daemonLogRing.Oldest(); !oldest.IsZero() {
		resp["oldest"] = oldest.UTC().Format(time.RFC3339)
	}
	b, _ := json.MarshalIndent(resp, "", "  ")
	return string(b), nil
}

// daemonQuery is a validated daemon_log request.
type daemonQuery struct {
	project, taskID string
	since           time.Time
	limit           int
}

// daemonLogQuery parses and scopes a daemon_log call: the rate limit, the
// ring and scrubber being present, and a project or task the key covers
// (another namespace's answers exactly as an unknown one).
func (s *Server) daemonLogQuery(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (daemonQuery, error) {
	var args daemonLogArgs
	if err := decodeToolArgs("daemon_log", raw, &args); err != nil {
		return daemonQuery{}, err
	}
	if err := s.gateLogTool(key, "daemon_log"); err != nil {
		return daemonQuery{}, err
	}
	if s.daemonLogRing == nil { // no scrubber needed: no free text is returned (review-20261008-33a5 F4)
		recordLogToolOutcome("daemon_log", "refused")
		return daemonQuery{}, errors.New("daemon_log is not available on this daemon")
	}
	q := daemonQuery{project: strings.TrimSpace(args.Project), taskID: strings.TrimSpace(args.TaskID), limit: args.Limit}
	if q.project != "" && !keyCoversProject(key, q.project) {
		recordLogToolOutcome("daemon_log", "not_found")
		return daemonQuery{}, fmt.Errorf("project %s not found", q.project)
	}
	if q.taskID != "" {
		task, err := s.taskRepo.Get(ctx, q.taskID)
		if err != nil || task == nil || !keyCoversProject(key, task.ProjectID) {
			recordLogToolOutcome("daemon_log", "not_found")
			return daemonQuery{}, fmt.Errorf("task %s not found", q.taskID)
		}
	}
	if args.Since != "" {
		t, err := time.Parse(time.RFC3339, args.Since)
		if err != nil {
			recordLogToolOutcome("daemon_log", "refused")
			return daemonQuery{}, errors.New("since must be RFC 3339")
		}
		q.since = t
	}
	if q.limit <= 0 {
		q.limit = daemonLogDefaultLimit
	}
	if q.limit > daemonLogMaxLimit {
		q.limit = daemonLogMaxLimit
	}
	return q, nil
}

// filterDaemonLines keeps the ring's lines the key may read and the query
// asks for, at most q.limit, returned oldest first. Its cost per call is
// bounded (security scan of f1eb16871): it walks a copy of the ring (at most
// 2000 lines) newest first and stops at the limit, returns no free text, and
// makes at most daemonLogMaxTaskLookups repository lookups in all. A line
// whose project needs a lookup past the cap is skipped and counted, never
// returned unscoped; an id whose own project needs a lookup past the cap, or
// is outside the namespace, is omitted (review-20261008-a4ef F1).
func (s *Server) filterDaemonLines(ctx context.Context, key *persistence.APIKey, q daemonQuery) ([]map[string]any, int, int) {
	r := &idResolver{s: s, ctx: ctx, tasks: map[string]string{}, execs: map[string]string{}}
	skipped := 0
	lines := s.daemonLogRing.Since(nil, true, q.since, 0)
	picked := make([]map[string]any, 0, q.limit)
	withheld := 0
	for i := len(lines) - 1; i >= 0 && len(picked) < q.limit; i-- {
		l := lines[i]
		if q.taskID != "" && l.TaskID != q.taskID {
			continue
		}
		project := l.ProjectID
		if project == "" && l.TaskID != "" {
			p, ok := r.task(l.TaskID)
			if !ok {
				skipped++
				continue
			}
			project = p
		}
		if project == "" || !keyCoversProject(key, project) || (q.project != "" && project != q.project) {
			continue
		}
		row, w := s.daemonLogRow(l, project, key, r)
		if w {
			withheld++
		}
		picked = append(picked, row)
	}
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return picked, withheld, skipped
}

// idResolver resolves task and execution ids to their projects for one
// daemon_log call, with one shared lookup budget.
type idResolver struct {
	s       *Server
	ctx     context.Context
	tasks   map[string]string
	execs   map[string]string
	lookups int
}

func (r *idResolver) budget() bool {
	if r.lookups >= daemonLogMaxTaskLookups {
		return false
	}
	r.lookups++
	return true
}

// task is the project of a task id; ok is false when the lookup budget is spent.
func (r *idResolver) task(id string) (string, bool) {
	if p, ok := r.tasks[id]; ok {
		return p, true
	}
	if r.s.taskRepo == nil || !r.budget() {
		return "", false
	}
	p := ""
	if t, err := r.s.taskRepo.Get(r.ctx, id); err == nil && t != nil {
		p = t.ProjectID
	}
	r.tasks[id] = p
	return p, true
}

// exec is the project of an execution id; ok is false when the budget is spent.
func (r *idResolver) exec(id string) (string, bool) {
	if p, ok := r.execs[id]; ok {
		return p, true
	}
	if r.s.executionRepo == nil || !r.budget() {
		return "", false
	}
	p := ""
	if e, err := r.s.executionRepo.Get(r.ctx, id); err == nil && e != nil {
		p = e.ProjectID
	}
	r.execs[id] = p
	return p, true
}

// daemonLogRow renders one ring line for an agent: the closed fields (time,
// level, component, project, the closed MCP code mapped from the numeric
// status) and the message only when it is one of the daemon's own allowlisted
// literals (daemonLogMessageAllowlist). Free text never leaves: every agent
// namespace project is a broker project, and a message can carry a task's raw
// error, another project's name, a URL or a path (security scan, information
// disclosure, 2026-10-08). A task or execution id is emitted only when its own
// project is in the key's namespace, and a server name only when it is a
// configured server of the line's project (review-20261008-a4ef F1): a line
// on a covered project can still name another project's task. The bool is
// true when a message was omitted.
func (s *Server) daemonLogRow(l logring.Line, project string, key *persistence.APIKey, r *idResolver) (map[string]any, bool) {
	row := map[string]any{"time": l.Time.UTC().Format(time.RFC3339), "level": l.Level,
		"component": l.Component, "project": project}
	if l.TaskID != "" {
		if p, ok := r.task(l.TaskID); ok && p != "" && keyCoversProject(key, p) {
			row["task_id"] = l.TaskID
		}
	}
	if l.ExecutionID != "" {
		if p, ok := r.exec(l.ExecutionID); ok && p != "" && keyCoversProject(key, p) {
			row["execution_id"] = l.ExecutionID
		}
	}
	if l.Server != "" && s.isProjectServer(project, l.Server) {
		row["server"] = l.Server
	}
	if l.Component == "mcp" && l.HTTPStatus != 0 {
		code, _ := agentadmin.ClassifyConnectionError(errLoggedFailure, l.HTTPStatus)
		row["code"] = string(code)
	}
	if daemonLogMessageAllowlist[l.Message] {
		row["message"] = l.Message
		return row, false
	}
	row["message_omitted"] = true
	return row, true
}

// isProjectServer reports whether name is a configured MCP server of project.
func (s *Server) isProjectServer(project, name string) bool {
	if s.projectRegistry == nil {
		return false
	}
	p := s.projectRegistry.GetProject(project)
	if p == nil {
		return false
	}
	for _, srv := range p.MCP.Servers {
		if srv.Name == name {
			return true
		}
	}
	return false
}

// errLoggedFailure stands for a failure known only by its logged status.
var errLoggedFailure = errors.New("logged connect failure")

// ---- task_explain ---------------------------------------------------------

func (s *Server) companionToolTaskExplain(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	var args taskIDArg
	if err := decodeToolArgs("task_explain", raw, &args); err != nil {
		return "", err
	}
	args.TaskID = strings.TrimSpace(args.TaskID)
	if args.TaskID == "" {
		return "", errors.New("task_id is required")
	}
	// The shared rate limit before any repository read (review-20261008-3e8e
	// F2), and any lookup failure answers as an unknown task: a driver's error
	// text never reaches an agent (F1).
	if err := s.gateLogTool(key, "task_explain"); err != nil {
		return "", err
	}
	task, err := s.taskRepo.Get(ctx, args.TaskID)
	if err != nil || task == nil || !keyCoversProject(key, task.ProjectID) {
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			s.logger.Warn().Err(err).Str("task_id", args.TaskID).Msg("task_explain: task lookup failed")
		}
		recordLogToolOutcome("task_explain", "not_found")
		return "", fmt.Errorf("task %s not found", args.TaskID)
	}
	if s.explainRenderer == nil {
		recordLogToolOutcome("task_explain", "refused")
		return "", errors.New("task_explain is not available on this daemon")
	}
	if s.brokerViewFor(key, task) {
		recordLogToolOutcome("task_explain", "ok")
		return brokerExplain(ctx, s, task), nil
	}
	st := s.liveLogs()
	if st.scrub == nil {
		recordLogToolOutcome("task_explain", "refused")
		return "", errors.New("task_explain is unavailable: the log scrubber could not be built")
	}
	// A copy of the renderer with no log fetcher: the companion view drops the
	// log tail anyway (companionExplainView's allowlist), so it is not read.
	// (Before GitHub #294 the daemon's fetcher was Executor.TaskLogs, which ran
	// podman logs; it is HeldTaskLog now.)
	renderer := *s.explainRenderer
	renderer.Logs = nil
	res, err := renderer.Render(ctx, task.ID)
	if err != nil {
		return "", fmt.Errorf("task %s not found", args.TaskID)
	}
	recordLogToolOutcome("task_explain", "ok")
	out, withheld := companionExplainView(task.ID, res.Inputs, st.scrub)
	if withheld > 0 {
		recordLogWithheld("task_explain", withheld)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// companionExplainView builds task_explain's companion answer from an
// ALLOWLIST of the renderer's inputs (review-20261008-33a5 F1): the task's
// status, workflow, class, the playbook entry (operator-written corpus text),
// each step's id, role, model, outcome and class, and last_error, scrubbed.
// Nothing else: not the rendered summary (it quotes step error detail), not
// the recent tool calls, not the log tail, not step error detail. last_error
// is the caller's own task's text, which status already returns to this key.
func companionExplainView(taskID string, in postmortem.RenderedInputs, scrub report.LineScrubber) (map[string]any, int) {
	withheld := 0
	lastError, w := scrub(in.LastError)
	if w {
		lastError, withheld = "[withheld: secret]", 1
	}
	steps := make([]map[string]any, 0, len(in.StepOutcomes))
	for _, o := range in.StepOutcomes {
		steps = append(steps, map[string]any{
			"step_id": o.StepID, "role": o.Role, "model": o.Model,
			"outcome": closedStepOutcome(o.Outcome), "error_class": closedStepClass(o.ErrorClass),
		})
	}
	out := map[string]any{
		"task_id":          taskID,
		"task_status":      in.TaskStatus,
		"workflow_id":      in.WorkflowID,
		"last_error_class": livepubsub.ClosedTaskClass(in.LastErrorClass),
		"last_error":       lastError,
		"step_outcomes":    steps,
	}
	if in.LastErrorClass == "" {
		delete(out, "last_error_class")
	}
	if in.Playbook != nil {
		out["playbook"] = in.Playbook
	}
	return out, withheld
}

// stepClassSet is the declared step error class vocabulary.
var stepClassSet = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range stepoutcome.ErrorClasses() {
		m[c] = true
	}
	return m
}()

// closedStepClass passes a declared step error class, else "unclassified"
// (review-20261008-b439 F2): the view does not rely on writers alone.
func closedStepClass(c string) string {
	if c == "" || stepClassSet[c] {
		return c
	}
	return stepoutcome.ClassUnclassified
}

// stepOutcomeShape is a plain lower-snake identifier, the shape of every
// stepoutcome.Outcome constant.
var stepOutcomeShape = regexp.MustCompile(`^[a-z][a-z_]{0,39}$`)

// closedStepOutcome passes an outcome shaped like the step vocabulary, else
// "unknown".
func closedStepOutcome(o string) string {
	if stepOutcomeShape.MatchString(o) {
		return o
	}
	return "unknown"
}

// brokerExplain is explain for a broker read: the closed error class and the
// playbook id (the task failure class the corpus is keyed on), nothing else.
func brokerExplain(ctx context.Context, s *Server, task *persistence.Task) string {
	class := ""
	if task.Status == persistence.TaskStatusFailed {
		class = classifyBrokerFailure(task)
	} else {
		class = s.brokerStatusErrorClass(ctx, task, s.brokerWorkflowOf(derefString(task.WorkflowID)))
	}
	playbook := ""
	if task.LastErrorClass != nil {
		playbook = livepubsub.ClosedTaskClass(*task.LastErrorClass)
	}
	b, _ := json.MarshalIndent(map[string]any{
		"task_id": task.ID, "status": string(task.Status), "error_class": class, "playbook_id": playbook,
	}, "", "  ")
	return string(b)
}

// ---- doctor ---------------------------------------------------------------

func (s *Server) mcpPendingSource() func() (int, []mcp.PendingServer) {
	if s.diagMCPPending != nil {
		return s.diagMCPPending
	}
	if doctorHandlers != nil {
		return doctorHandlers.mcpPending
	}
	return nil
}

func (s *Server) mcpNoGrantSource() func() []mcpconnect.NoGrant {
	if s.diagMCPNoGrant != nil {
		return s.diagMCPNoGrant
	}
	if doctorHandlers != nil {
		return doctorHandlers.mcpNoGrant
	}
	return nil
}

func (s *Server) companionToolDoctor(ctx context.Context, key *persistence.APIKey) (string, error) {
	if err := s.gateLogTool(key, "doctor"); err != nil {
		return "", err
	}
	servers := []map[string]any{}
	if src := s.mcpPendingSource(); src != nil {
		_, pending := src()
		for _, p := range pending {
			if !keyCoversProject(key, p.ProjectID) {
				continue
			}
			code, _ := agentadmin.ClassifyConnectionError(causeOrLogged(p.Cause), p.HTTPStatus)
			servers = append(servers, map[string]any{
				"project": p.ProjectID, "server": p.Server, "state": "pending", "code": string(code),
				"since": p.Since.UTC().Format(time.RFC3339), "attempts": p.Attempts,
			})
		}
	}
	if src := s.mcpNoGrantSource(); src != nil {
		for _, g := range src() {
			if !keyCoversProject(key, g.ProjectID) {
				continue
			}
			servers = append(servers, map[string]any{
				"project": g.ProjectID, "server": g.Server, "state": "withheld", "code": string(agentadmin.ConnCredentialMissing),
			})
		}
	}
	sort.SliceStable(servers, func(i, j int) bool {
		if servers[i]["state"] != servers[j]["state"] {
			return servers[i]["state"] == "pending"
		}
		return fmt.Sprint(servers[i]["project"], servers[i]["server"]) < fmt.Sprint(servers[j]["project"], servers[j]["server"])
	})
	failed, truncated, scanErr := s.namespaceFailedByClass(ctx, key)
	recordLogToolOutcome("doctor", "ok")
	b, _ := json.MarshalIndent(map[string]any{
		"mcp_servers":            servers,
		"failed_tasks_by_class":  failed,
		"failed_tasks_truncated": truncated,
		// A failed scan is not a bound hit (review-20261008-202b minor).
		"failed_tasks_unavailable": scanErr,
		"window":                   doctorFailedWindow.String(),
	}, "", "  ")
	return string(b), nil
}

func causeOrLogged(err error) error {
	if err == nil {
		return errLoggedFailure
	}
	return err
}

// namespaceFailedByClass counts the namespace's tasks that failed in the
// window, by closed task failure class. It is a bounded scan (no GROUP BY in
// the repository, and this lane adds no SQL): at most doctorMaxProjects
// projects and doctorFailedScanLimit tasks; truncated reports a hit on either
// bound instead of undercounting silently (review-20261008-a4ef F3).
func (s *Server) namespaceFailedByClass(ctx context.Context, key *persistence.APIKey) (map[string]int, bool, bool) {
	out := map[string]int{}
	if s.taskRepo == nil || s.projectRegistry == nil {
		return out, false, true
	}
	var projects []string
	for _, p := range s.projectRegistry.ListProjects() {
		if keyCoversProject(key, p.ID) {
			projects = append(projects, p.ID)
		}
	}
	if len(projects) == 0 {
		return out, false, false
	}
	sort.Strings(projects)
	truncated := false
	if len(projects) > doctorMaxProjects {
		projects, truncated = projects[:doctorMaxProjects], true
	}
	failed := persistence.TaskStatusFailed
	since := time.Now().Add(-doctorFailedWindow)
	tasks, err := s.taskRepo.List(ctx, persistence.TaskFilter{ProjectIDs: projects, Status: &failed, FailedSince: &since, PageSize: doctorFailedScanLimit})
	if err != nil {
		return out, false, true // a failed scan says only that (review-20261008-f42d F5)
	}
	if len(tasks) >= doctorFailedScanLimit {
		truncated = true
	}
	for _, t := range tasks {
		cls := persistence.TaskFailureClassUnknown
		if t.LastErrorClass != nil {
			cls = livepubsub.ClosedTaskClass(*t.LastErrorClass)
		}
		out[cls]++
	}
	return out, truncated, false
}
