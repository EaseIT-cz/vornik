package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/ctxerr"
	"vornik.io/vornik/internal/persistence"
)

func skillBodyHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Server-side skill distiller — E1 of the learning loop (LLD 2026-07-07-
// knowledge-skill-learning-loop-design §E1). After a Vornik task
// completes successfully, a bounded LLM pass looks at the task's goal +
// result and, if it finds durable reusable know-how, proposes a DRAFT
// skill (a human still approves before it fires). This covers work
// Vornik itself executes; companion-session know-how is captured
// client-side (E2) because Vornik can't see those transcripts.
//
// Cost control: per-project rate limit + the model returns {skip:true}
// for trivial/one-off tasks, so most autonomous ticks distil nothing.

const (
	skillDistillMaxPerWindow = 3
	skillDistillWindow       = time.Hour
	skillDistillBodyCap      = 65536
)

// distillCandidate is the model's structured verdict.
type distillCandidate struct {
	Skip        bool     `json:"skip"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	Domain      string   `json:"domain"`
	Roles       []string `json:"roles"`
}

const skillDistillSystemPrompt = `You review a COMPLETED task and decide whether it produced DURABLE, REUSABLE, project-relevant know-how worth saving as a "skill" (an instructional procedure a future agent should follow).

Return STRICT JSON only, no prose. Either:
  {"skip": true}
when the task is trivial, one-off, or produced nothing reusable (most autonomous/feed tasks are skip), OR:
  {"skip": false, "name": "kebab-case-slug", "description": "one line: WHEN to apply this", "body": "# Title\nActionable Markdown steps / checks / anti-patterns", "domain": "software|networking|...", "roles": []}
Set "roles" to swarm role names the skill applies to, or [] for any role. Never include secrets. Prefer skip unless the procedure is genuinely worth reusing.`

// skillDistillLimiter is an in-memory per-project rate limiter.
type skillDistillLimiter struct {
	mu     sync.Mutex
	starts map[string][]time.Time
}

func (l *skillDistillLimiter) allow(project string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.starts == nil {
		l.starts = make(map[string][]time.Time)
	}
	cutoff := now.Add(-skillDistillWindow)
	kept := l.starts[project][:0]
	for _, t := range l.starts[project] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= skillDistillMaxPerWindow {
		l.starts[project] = kept
		return false
	}
	l.starts[project] = append(kept, now)
	return true
}

// maybeDistillSkill runs the distiller for a successfully-completed task.
// Best-effort and nil-safe: any miss (no LLM wired, rate-limited, model
// says skip, parse error, duplicate) proposes nothing, and each is logged and
// counted on skill_distill_skipped_total{reason} (GitHub #369). Intended
// to run async so it never delays task completion.
func (e *Executor) maybeDistillSkill(ctx context.Context, task *persistence.Task, result string) {
	if e.distillerLLM == nil || e.skillRepo == nil || task == nil {
		return
	}
	reason, project, proven, err := e.distillRefusal(ctx, task)
	if reason != "" || proven == nil {
		if reason == "" {
			reason = "lookup_error"
		}
		e.logger.Info().Err(err).Str("task_id", task.ID).Str("project_id", project).
			Str("reason", reason).Msg("skill distiller: task not distilled")
		if e.metrics != nil {
			e.metrics.RecordSkillDistillSkipped(reason)
		}
		return
	}
	// From here on the distiller works only from the proven row.
	task = proven
	if !e.distillLimiter.allow(task.ProjectID, time.Now().UTC()) {
		e.distillSkipped(task, "rate_limited", nil)
		return
	}
	goal := strings.TrimSpace(taskGoal(task))
	if goal == "" && strings.TrimSpace(result) == "" {
		e.distillSkipped(task, "empty_input", nil)
		return
	}
	user := "TASK GOAL:\n" + truncateForDistill(goal, 4000) + "\n\nTASK RESULT:\n" + truncateForDistill(result, 6000)
	resp, err := e.distillerLLM.Complete(ctx, []chat.Message{
		{Role: "system", Content: skillDistillSystemPrompt},
		{Role: "user", Content: user},
	})
	if err != nil || resp == nil || len(resp.Choices) == 0 {
		// Logged and counted like every other skip here, so "the model is
		// down" no longer looks like "nothing worth distilling"
		// (outcome-quality LLD §S5 #18, GitHub #142).
		skip := "llm_empty"
		if err != nil {
			skip = "llm_" + string(ctxerr.Classify(ctx, err))
		}
		e.logger.Warn().Err(err).Str("task_id", task.ID).Str("project_id", task.ProjectID).
			Str("reason", skip).Msg("skill distiller: model call failed; task not distilled")
		if e.metrics != nil {
			e.metrics.RecordSkillDistillSkipped(skip)
		}
		return
	}
	cand, ok := parseDistillCandidate(resp.Choices[0].Message.Content)
	if !ok {
		e.distillSkipped(task, "unparseable", nil)
		return
	}
	if cand.Skip {
		e.distillSkipped(task, "model_skip", nil)
		return
	}
	cand.Name = strings.TrimSpace(cand.Name)
	cand.Description = strings.TrimSpace(cand.Description)
	if cand.Name == "" || cand.Description == "" || strings.TrimSpace(cand.Body) == "" {
		e.distillSkipped(task, "incomplete_candidate", nil)
		return
	}
	if len(cand.Body) > skillDistillBodyCap {
		cand.Body = cand.Body[:skillDistillBodyCap]
	}
	// Dedup, first pass: an exact same-named skill in this project (any
	// scope). Cheap, and it catches the trivial case without an embedding.
	//
	// It catches ONLY that. This comment used to claim it "avoids
	// near-duplicate drafts", which is precisely what a case-insensitive
	// equality cannot do: `prague-restaurant-feed-refresh` and
	// `prague-restaurants-feed-refresh` differ by one character, and the
	// second was proposed, approved, and never fired once while the first
	// accumulated 23 firings.
	existing, lerr := e.skillRepo.List(ctx, task.ProjectID, persistence.SkillListFilter{})
	if lerr != nil {
		// Propose anyway, as the near-duplicate check below does on an
		// outage: the repository's own name-conflict check still catches an
		// exact duplicate at Create (GitHub #369).
		e.logger.Warn().Err(lerr).Str("task_id", task.ID).Str("project_id", task.ProjectID).
			Msg("skill distiller: exact-name check failed; proposing anyway")
	}
	for _, s := range existing {
		if strings.EqualFold(s.Name, cand.Name) {
			e.distillSkipped(task, "duplicate_name", nil)
			return
		}
	}
	sum := skillBodyHash(cand.Body)
	skill := &persistence.Skill{
		ID:           persistence.GenerateID("skill"),
		ProjectID:    task.ProjectID,
		Name:         cand.Name,
		Description:  cand.Description,
		Body:         cand.Body,
		BodySHA256:   sum,
		Domain:       strings.TrimSpace(cand.Domain),
		Roles:        cand.Roles,
		Maturity:     persistence.SkillMaturityDraft,
		Version:      1,
		OriginClient: "vornik-distiller",
		OriginTask:   task.ID,
	}
	// Dedup, second pass: the SEMANTIC gate (LLD §12.2). It has existed since
	// the knowledge-skill store shipped and was wired into exactly one
	// entrypoint — the companion `skill_propose` MCP tool — while this path
	// produced 105 of the reference deployment's 128 skills, 69 of which never
	// fired. A gate on one door out of several is the shape that let the
	// config assistant's mayAutoApply through on a different surface.
	//
	// SILENT SKIP, not a block-and-ask: this path is unattended, so there is
	// nobody to answer "supersedes or confirm_distinct?". Skipping matches what
	// the exact-name check above already does; the log line and the metric are
	// what make the skip rate visible, so "the gate is working" stays
	// distinguishable from "the distiller stopped proposing".
	if e.dupeChecker != nil {
		matches, derr := e.dupeChecker.NearDuplicateSkills(ctx, skill)
		switch {
		case derr != nil:
			// An embedder outage must not silence the distiller. §12.2 makes
			// the same choice on the MCP path; inheriting it beats inventing a
			// stricter rule here.
			e.logger.Warn().Err(derr).Str("name", skill.Name).
				Msg("skill distiller: near-duplicate check failed; proposing anyway")
		case len(matches) > 0:
			e.logger.Info().
				Str("name", skill.Name).
				Str("nearest", matches[0].Name).
				Float64("score", matches[0].Score).
				Str("reason", matches[0].Reason).
				Int("matches", len(matches)).
				Str("task_id", task.ID).
				Msg("skill distiller: draft skipped as a near-duplicate of an existing skill")
			if e.metrics != nil {
				e.metrics.RecordSkillDistillSkipped(matches[0].Reason)
			}
			return
		}
	}

	if err := e.skillRepo.Create(ctx, skill); err != nil {
		if errors.Is(err, persistence.ErrSkillNameConflict) {
			e.distillSkipped(task, "duplicate_name", nil)
		} else {
			e.distillSkipped(task, "persist_error", err)
		}
		return
	}
	e.logger.Info().Str("skill_id", skill.ID).Str("name", skill.Name).
		Str("task_id", task.ID).Msg("skill distiller: proposed draft from completed task")
}

// distillSkipped logs and counts one post-gate return of the distiller
// (GitHub #369, outcome-quality LLD §S8.11 row D), so "the model said skip"
// and "the run never reached the model" are told apart. The reasons a broken
// model, prompt or store produces log at Warn; the ordinary ones at Info.
func (e *Executor) distillSkipped(task *persistence.Task, reason string, err error) {
	ev := e.logger.Info()
	switch reason {
	case "unparseable", "incomplete_candidate", "persist_error":
		ev = e.logger.Warn()
	}
	ev.Err(err).Str("task_id", task.ID).Str("project_id", task.ProjectID).
		Str("reason", reason).Msg("skill distiller: nothing proposed")
	if e.metrics != nil {
		e.metrics.RecordSkillDistillSkipped(reason)
	}
}

// distillRefusal names why a task must not be distilled, or "" when it may
// be (agent-administered design §18.9 item 2). A broker task's result is
// third-party content, and a draft skill is standing instructions for the
// project's future agents, so a broker workflow (whatever its provenance:
// first_party is the operator's claim, not something computable yet) and any
// task of a broker project (broker §5.2b) are never distilled. Everything is
// re-derived here, not taken from the caller: this runs in a goroutine after
// the success path returns, so the row and the config may have moved since.
func (e *Executor) distillRefusal(ctx context.Context, task *persistence.Task) (reason, project string, proven *persistence.Task, err error) {
	project = task.ProjectID
	if e.taskRepo == nil {
		return "lookup_error", project, nil, errors.New("no task repository")
	}
	row, err := e.taskRepo.Get(ctx, task.ID)
	if err != nil || row == nil {
		// Fail closed, but apart from a real status change: a storage fault
		// must not read as task churn (review f8b7 F4).
		return "lookup_error", project, nil, err
	}
	project = row.ProjectID
	if row.Status != persistence.TaskStatusCompleted {
		return "not_completed", project, nil, nil
	}
	if e.workflows == nil {
		return "unresolved", project, nil, nil
	}
	p := e.workflows.GetProject(row.ProjectID)
	if p == nil {
		return "unresolved", project, nil, nil
	}
	if p.Broker {
		return "broker_project", project, nil, nil
	}
	workflowID := p.DefaultWorkflowID
	if row.WorkflowID != nil && *row.WorkflowID != "" && *row.WorkflowID != "-" {
		workflowID = *row.WorkflowID
	}
	// A workflow gone from config cannot be shown to be ordinary, any more
	// than a project can: fail closed (review f8b7 F1).
	wf := e.workflows.GetWorkflow(workflowID)
	if wf == nil {
		return "unresolved", project, nil, nil
	}
	if wf.Broker != nil {
		return "broker_workflow", project, nil, nil
	}
	if r := distillSystemStepRefusal(wf); r != "" {
		return r, project, nil, nil
	}
	if reason, err = e.distillTaintRefusal(ctx, row); reason != "" {
		return reason, project, nil, err
	}
	// The proof is about THIS row; the caller distils from it, not from its
	// own copy of the task (review a8a5 F11).
	return "", project, row, nil
}

func parseDistillCandidate(raw string) (distillCandidate, bool) {
	s := strings.TrimSpace(raw)
	// Tolerate a ```json fence.
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j >= i {
			s = s[i : j+1]
		}
	}
	var c distillCandidate
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return distillCandidate{}, false
	}
	return c, true
}

// taskGoal best-effort extracts the human goal/prompt from a task's
// JSON payload (the field name varies across creation paths).
func taskGoal(task *persistence.Task) string {
	if len(task.Payload) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(task.Payload, &m); err != nil {
		return ""
	}
	for _, k := range []string{"prompt", "goal", "description", "task", "input"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncateForDistill(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// SkillDupeMatch is one near-duplicate the gate found. Declared here rather
// than reused from internal/api because api imports this package — the
// dependency only runs one way, so the container adapts across the seam.
type SkillDupeMatch struct {
	Name   string
	Score  float64
	Reason string
}

// SkillDupeChecker scores a proposed skill against the existing catalogue.
// Optional: a nil checker leaves the exact-name dedup above as the only gate,
// which is the pre-2026-09-16 behaviour.
type SkillDupeChecker interface {
	NearDuplicateSkills(ctx context.Context, candidate *persistence.Skill) ([]SkillDupeMatch, error)
}
