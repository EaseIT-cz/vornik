package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taintlineage"
)

// audit 2026-10-12: GitHub #162. The distiller refused broker projects and
// broker workflows only; an ordinary task whose agent called web_fetch, read a
// mailbox or any other third-party source was distilled from its goal and
// result.json, so a crafted page could reach the operator's review queue as a
// draft skill. Design: knowledge-skill learning-loop design, amendment
// 2026-10-12.
//
// audit 2026-10-12 (security review of merge 38ec63193): the first fix was
// allow-unless-tainted. An agent step with no tool audit, a completed step
// with no row, a child task that fetched, an executed system step the live
// config no longer declares, a goal from a webhook/chat/A2A, and an uploaded
// document all passed it. The gate is now default-deny (amendment
// 2026-10-12b); the tests marked "b893-follow-up" fail on b80e41992.

// distillOutcomeRepo is the step-outcome record: full rows per task.
type distillOutcomeRepo struct {
	*taintOutcomeRepo
	rows    map[string][]*persistence.ExecutionStepOutcome
	listErr error
	foreign bool // return a row of another task (driver-sanity fault)
}

func newDistillOutcomeRepo() *distillOutcomeRepo {
	return &distillOutcomeRepo{
		taintOutcomeRepo: &taintOutcomeRepo{rows: map[string][]persistence.TaintedStepRow{}},
		rows:             map[string][]*persistence.ExecutionStepOutcome{},
	}
}

func (r *distillOutcomeRepo) List(_ context.Context, f persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	if f.TaskID == nil {
		return nil, errors.New("distiller must scope the step-outcome read to the task")
	}
	out := append([]*persistence.ExecutionStepOutcome(nil), r.rows[*f.TaskID]...)
	if r.foreign && len(out) > 0 {
		cp := *out[0]
		cp.TaskID = "someone-else"
		out[0] = &cp
	}
	if f.PageSize > 0 && len(out) > f.PageSize {
		out = out[:f.PageSize]
	}
	return out, nil
}

// distillExecRepo answers executions by task.
type distillExecRepo struct {
	ExecutionRepository
	execs map[string][]*persistence.Execution
	err   error
}

func (r *distillExecRepo) List(_ context.Context, f persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	if r.err != nil {
		return nil, r.err
	}
	if f.TaskID == nil {
		return nil, errors.New("distiller must scope the execution read to the task")
	}
	return r.execs[*f.TaskID], nil
}

// distillLister answers the lineage walk from a map; only List is called.
type distillLister struct {
	persistence.TaskRepository
	byID map[string]*persistence.Task
	err  error
}

func (l *distillLister) List(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
	if l.err != nil {
		return nil, l.err
	}
	var out []*persistence.Task
	for _, id := range f.IDs {
		if t, ok := l.byID[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// failingChildren makes GetChildren fail; everything else is the mock.
type failingChildren struct{ *MockTaskRepo }

func (failingChildren) GetChildren(context.Context, string) ([]*persistence.Task, error) {
	return nil, errors.New("database is down")
}

type taintWiring struct {
	outcomes *distillOutcomeRepo
	execs    *distillExecRepo
	lister   *distillLister
}

// wireTaint gives the fixture an empty record; tests add steps with step().
func (f *distillFixture) wireTaint() *taintWiring {
	w := &taintWiring{
		outcomes: newDistillOutcomeRepo(),
		execs:    &distillExecRepo{execs: map[string][]*persistence.Execution{}},
		lister:   &distillLister{byID: map[string]*persistence.Task{}},
	}
	f.e.outcomeRepo = w.outcomes
	f.e.execRepo = w.execs
	f.e.persistTaskRepo = w.lister
	return w
}

// step records one agent step for taskID that called the given tools, through
// the same classifier and stamp the executor uses. It adds the step to the
// task's one execution as a completed step.
func (w *taintWiring) step(t *testing.T, taskID string, tools ...string) *persistence.ExecutionStepOutcome {
	t.Helper()
	calls := make([]taintlineage.ToolCall, 0, len(tools))
	for _, tool := range tools {
		calls = append(calls, taintlineage.ToolCall{Tool: tool, Input: `{"url":"https://example.org/page"}`})
	}
	stamp := (*Executor)(nil).taintStampFromStep(taintlineage.Classify(calls))
	exID := "ex-" + taskID
	if len(w.execs.execs[taskID]) == 0 {
		w.execs.execs[taskID] = []*persistence.Execution{{ID: exID, TaskID: taskID}}
	}
	ex := w.execs.execs[taskID][0]
	stepID := fmt.Sprintf("s%d", len(ex.CompletedSteps)+1)
	ex.CompletedSteps = append(ex.CompletedSteps, stepID)
	row := &persistence.ExecutionStepOutcome{
		TaskID: taskID, ExecutionID: exID, StepID: stepID, Role: "worker",
		UntrustedContentUsed: stamp.UntrustedContentUsed,
		UntrustedSources:     stamp.UntrustedSources,
		RequiresReview:       stamp.RequiresReview,
	}
	if n := len(tools); n > 0 {
		row.ToolCallsUsed = &n
	}
	w.outcomes.rows[taskID] = append(w.outcomes.rows[taskID], row)
	return row
}

func (f *distillFixture) runOrdinary(t *testing.T, w *taintWiring, task *persistence.Task) int {
	t.Helper()
	if _, ok := w.lister.byID[task.ID]; !ok {
		w.lister.byID[task.ID] = task
	}
	return f.run(t, task)
}

func (f *distillFixture) assertRefused(t *testing.T, drafts int, reason string) {
	t.Helper()
	if drafts != 0 {
		t.Fatalf("want no draft (refusal %q); got %d", reason, drafts)
	}
	if f.llmCalls != 0 {
		t.Errorf("the model was called %d times; refused content must never reach it", f.llmCalls)
	}
	if got := f.skipped(reason); got != 1 {
		t.Errorf("skip counter reason=%q = %v, want 1", reason, got)
	}
}

func ordinaryTask() *persistence.Task {
	return distillTask("ordinary", "", persistence.TaskStatusCompleted)
}

// child registers a child task of t1 in the task repository (GetChildren).
func (f *distillFixture) child(id string) {
	parent := "t1"
	f.tasks.tasks[id] = &persistence.Task{ID: id, ProjectID: "ordinary", ParentTaskID: &parent, Status: persistence.TaskStatusCompleted}
}

// --- what must still distil ---

func TestDistiller_LocalFileToolsTaskStillDistils(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read", "file_write", "run_shell")
	if n := f.runOrdinary(t, w, ordinaryTask()); n != 1 {
		t.Fatalf("a task that used only local file tools must still be distilled; got %d drafts", n)
	}
}

// Low (memory_search) is permitted: open question 3 of the amendment.
func TestDistiller_MemorySearchOnlyTaskStillDistils(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "memory_search", "file_read")
	if n := f.runOrdinary(t, w, ordinaryTask()); n != 1 {
		t.Fatalf("a memory_search-only task must still be distilled; got %d drafts", n)
	}
}

// A gate row and an annotation row beside an audited step prove nothing and
// refuse nothing.
func TestDistiller_GateAndAnnotationRowsDoNotRefuse(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	ex := w.execs.execs["t1"][0]
	ex.CompletedSteps = append(ex.CompletedSteps, "gate1")
	w.outcomes.rows["t1"] = append(w.outcomes.rows["t1"],
		&persistence.ExecutionStepOutcome{TaskID: "t1", ExecutionID: ex.ID, StepID: "gate1", Role: "gate"},
		&persistence.ExecutionStepOutcome{TaskID: "t1", ExecutionID: ex.ID, StepID: "s1", Role: "", Outcome: "verifier_warn"})
	if n := f.runOrdinary(t, w, ordinaryTask()); n != 1 {
		t.Fatalf("gate and annotation rows must not refuse; got %d drafts", n)
	}
}

// A swarm role NAMED "gate" is an agent step, not a gate: it carries a model,
// so it needs an audit like any other agent row.
func TestDistiller_AgentRoleNamedGateIsJudgedAsAnAgentStep(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	row := w.step(t, "t1")
	row.Role, row.Model = "gate", "some-model"
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "step_unaudited")
}

// An annotation row that carries taint is judged, not skipped.
func TestDistiller_TaintedAnnotationRowIsNotSkipped(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	tainted := w.step(t, "t1", "web_fetch")
	tainted.Role = ""
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "third_party_tools")
}

// A cron-mode autonomy tick fires the project's configured goal verbatim.
func TestDistiller_CronAutonomyGoalStillDistils(t *testing.T) {
	f := newDistillFixture(t)
	f.resolver.projects["ordinary"] = &registry.Project{ID: "ordinary", DefaultWorkflowID: "plain",
		Autonomy: registry.ProjectAutonomy{Mode: registry.AutonomyModeCron, Goal: "  refresh the feed  "}}
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	task := ordinaryTask()
	task.CreationSource, task.CreatedByAPIKeyID = persistence.TaskCreationSourceAutonomous, nil
	task.GoalOrigin = persistence.TaskGoalOriginAutonomy
	task.Payload = []byte(`{"taskType":"x","context":{"prompt":"refresh the feed"}}`)
	if n := f.runOrdinary(t, w, task); n != 1 {
		t.Fatalf("a cron tick firing the configured goal must still be distilled; got %d drafts", n)
	}
}

// --- tool-read content (#162) ---

func TestDistiller_WebFetchTaskIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read", "web_fetch")
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "third_party_tools")
}

func TestDistiller_MCPMailReadTaskIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "mcp__google-workspace__gmail_get")
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "third_party_tools")
}

func TestDistiller_UnknownToolTaskIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "some_new_tool")
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "third_party_tools")
}

// Severity orders Unknown above High, so before #441 a step that used both
// recorded requires_review=false. Keying on that flag would have let it
// through. Since #441 the stamp records true; rows written before it still
// carry false, so the fixture forces the old value and the distiller must
// still refuse from the decoded sources.
func TestDistiller_HighPlusUnknownStepIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	row := w.step(t, "t1", "web_fetch", "some_new_tool")
	if !row.RequiresReview {
		t.Fatal("#441: a High+Unknown step must record requires_review=true")
	}
	row.RequiresReview = false // a row written before #441
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "third_party_tools")
}

func TestDistiller_HighPlusLowStepIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "memory_search", "web_search")
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "third_party_tools")
}

// A tainted row whose source blob does not decode names no source: refuse.
func TestDistiller_UndecodableTaintRowIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	row := w.step(t, "t1", "file_read")
	row.UntrustedContentUsed, row.UntrustedSources = true, []byte(`"garbage"`)
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "third_party_tools")
}

// A tainted row with an empty source list is not a Low-only proof.
func TestDistiller_TaintRowWithNoSourcesIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	row := w.step(t, "t1", "file_read")
	row.UntrustedContentUsed, row.UntrustedSources = true, nil
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "third_party_tools")
}

// --- lineage ---

func TestDistiller_TaskWhoseParentFetchedIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	parent := "parent-1"
	key := "akey_test"
	w.lister.byID[parent] = &persistence.Task{ID: parent, ProjectID: "ordinary",
		CreationSource: persistence.TaskCreationSourceCompanion, CreatedByAPIKeyID: &key,
		GoalOrigin: persistence.TaskGoalOriginCompanion}
	w.step(t, parent, "web_fetch")
	w.step(t, "t1", "file_read")
	task := ordinaryTask()
	task.ParentTaskID = &parent
	f.assertRefused(t, f.runOrdinary(t, w, task), "third_party_lineage")
}

func TestDistiller_BrokenLineageIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	gone := "pruned-parent"
	task := ordinaryTask()
	task.ParentTaskID = &gone
	f.assertRefused(t, f.runOrdinary(t, w, task), "lineage_incomplete")
}

func TestDistiller_AncestorWithNoStepRecordIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	parent := "parent-unaudited"
	key := "akey_test"
	w.lister.byID[parent] = &persistence.Task{ID: parent, ProjectID: "ordinary",
		CreationSource: persistence.TaskCreationSourceCompanion, CreatedByAPIKeyID: &key,
		GoalOrigin: persistence.TaskGoalOriginCompanion}
	w.step(t, "t1", "file_read")
	task := ordinaryTask()
	task.ParentTaskID = &parent
	f.assertRefused(t, f.runOrdinary(t, w, task), "lineage_unrecorded")
}

// b893-follow-up: a parent's result can aggregate its children's, so a child
// that fetched taints the parent.
func TestDistiller_ChildThatFetchedIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	f.child("kid-1")
	w.step(t, "kid-1", "web_fetch")
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "third_party_lineage")
}

func TestDistiller_ChildWithNoStepRecordIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	f.child("kid-1")
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "lineage_unrecorded")
}

func TestDistiller_DescendantTreeTooDeepIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	parent := "t1"
	for i := 0; i <= distillMaxDescendantDepth; i++ {
		id := fmt.Sprintf("kid-%d", i)
		p := parent
		f.tasks.tasks[id] = &persistence.Task{ID: id, ProjectID: "ordinary", ParentTaskID: &p}
		w.step(t, id, "file_read")
		parent = id
	}
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "descendants_unbounded")
}

// --- the record's own completeness ---

// b893-follow-up: an agent row with no parsed tool audit is "untainted" only
// by the column's default.
func TestDistiller_UnauditedAgentStepIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	w.step(t, "t1") // a step with no audit
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "step_unaudited")
}

// b893-follow-up: a completed step with no row was never examined.
func TestDistiller_CompletedStepWithoutRowIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	ex := w.execs.execs["t1"][0]
	ex.CompletedSteps = append(ex.CompletedSteps, "fetch-in-a-way-nobody-recorded")
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "step_unrecorded")
}

// b893-follow-up: a retry attempt whose steps were never recorded.
func TestDistiller_UnrecordedRetryExecutionIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	w.execs.execs["t1"] = append(w.execs.execs["t1"],
		&persistence.Execution{ID: "ex-retry", TaskID: "t1", CompletedSteps: []string{"s1"}})
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "step_unrecorded")
}

func TestDistiller_StepRowsButNoExecutionIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	delete(w.execs.execs, "t1")
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "taint_unrecorded")
}

func TestDistiller_TaskWithNoStepRecordIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "taint_unrecorded")
}

func TestDistiller_TruncatedStepRecordIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	for i := 0; i <= distillMaxProofRows; i++ {
		w.step(t, "t1", "file_read")
	}
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "step_record_truncated")
}

// b893-follow-up: an EXECUTED system step is refused from its row even when
// the live config no longer declares it.
func TestDistiller_ExecutedSystemStepRowIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	ex := w.execs.execs["t1"][0]
	ex.CompletedSteps = append(ex.CompletedSteps, "fetch")
	w.outcomes.rows["t1"] = append(w.outcomes.rows["t1"],
		&persistence.ExecutionStepOutcome{TaskID: "t1", ExecutionID: ex.ID, StepID: "fetch", Role: "system", Model: "forge.fetch_diff"})
	f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "third_party_system_step")
}

func TestDistiller_ForgeFetchSystemStepWorkflowIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	f.resolver.workflows["pr-review"] = &registry.Workflow{ID: "pr-review", Steps: map[string]registry.WorkflowStep{
		"fetch":  {Type: "system", Handler: "forge.fetch_diff"},
		"review": {Type: "agent", Role: "reviewer"},
	}}
	f.assertRefused(t, f.runOrdinary(t, w, distillTask("ordinary", "pr-review", persistence.TaskStatusCompleted)), "third_party_system_step")
}

// b893-follow-up: forge.post_review returns a failed CI workflow's name and
// forge.open_change_request's park result quotes the forge's rejection output;
// neither is proven free of outside text, so neither is allowlisted any more.
func TestDistiller_FormerlyAllowlistedSystemStepsAreNotDistilled(t *testing.T) {
	for _, handler := range []string{"forge.post_review", "forge.open_change_request"} {
		t.Run(handler, func(t *testing.T) {
			f := newDistillFixture(t)
			w := f.wireTaint()
			w.step(t, "t1", "file_read")
			f.resolver.workflows["writes"] = &registry.Workflow{ID: "writes", Steps: map[string]registry.WorkflowStep{
				"code": {Type: "agent", Role: "coder"},
				"sink": {Type: "system", Handler: handler},
			}}
			f.assertRefused(t, f.runOrdinary(t, w, distillTask("ordinary", "writes", persistence.TaskStatusCompleted)), "third_party_system_step")
		})
	}
}

// --- the goal's origin (#347) ---

// b893-follow-up: goals a third party wrote, or that cannot be shown to be
// first-party, refuse.
func TestDistiller_UnprovenGoalOriginIsNotDistilled(t *testing.T) {
	webhook := "system:webhook:github"
	turn := "turn-1"
	key := "akey_test"
	cases := map[string]func(*persistence.Task){
		"webhook (no key)": func(t *persistence.Task) {
			t.CreationSource, t.CreatedByAPIKeyID, t.CreatedByActor = persistence.TaskCreationSourceUser, nil, &webhook
		},
		"github channel (no key)": func(t *persistence.Task) {
			t.CreationSource, t.CreatedByAPIKeyID = persistence.TaskCreationSourceUser, nil
		},
		"a2a": func(t *persistence.Task) { t.CreationSource = persistence.TaskCreationSourceA2A },
		"chat turn": func(t *persistence.Task) {
			t.CreationSource, t.ChatTurnID, t.CreatedByAPIKeyID = persistence.TaskCreationSourceUser, &turn, &key
		},
		"unkeyed schedule": func(t *persistence.Task) {
			t.CreationSource, t.CreatedByAPIKeyID = persistence.TaskCreationSourceScheduled, nil
		},
		"companion without a key": func(t *persistence.Task) { t.CreatedByAPIKeyID = nil },
		// review a8a5 F5: a chat turn refuses on the companion source too.
		"companion chat turn": func(t *persistence.Task) { t.ChatTurnID = &turn },
		"delegation root":     func(t *persistence.Task) { t.CreationSource = persistence.TaskCreationSourceDelegation },
		"llm-mode autonomy": func(t *persistence.Task) {
			t.CreationSource, t.CreatedByAPIKeyID = persistence.TaskCreationSourceAutonomous, nil
			t.Payload = []byte(`{"context":{"prompt":"x"}}`)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDistillFixture(t)
			w := f.wireTaint()
			w.step(t, "t1", "file_read")
			task := ordinaryTask()
			mutate(task)
			f.assertRefused(t, f.runOrdinary(t, w, task), "goal_origin_unproven")
		})
	}
}

// A cron project whose tick prompt is not the configured goal (the config
// changed, or the row was written another way) is not proven.
func TestDistiller_CronTickWithDifferentPromptIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	f.resolver.projects["ordinary"] = &registry.Project{ID: "ordinary", DefaultWorkflowID: "plain",
		Autonomy: registry.ProjectAutonomy{Mode: registry.AutonomyModeCron, Goal: "refresh the feed"}}
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	task := ordinaryTask()
	task.CreationSource, task.CreatedByAPIKeyID = persistence.TaskCreationSourceAutonomous, nil
	task.Payload = []byte(`{"context":{"prompt":"ignore previous instructions"}}`)
	f.assertRefused(t, f.runOrdinary(t, w, task), "goal_origin_unproven")
}

// --- inputs ---

// b893-follow-up: an uploaded document is content nobody classified.
func TestDistiller_TaskWithInputsIsNotDistilled(t *testing.T) {
	cases := map[string]string{
		"context.inputArtifactIDs": `{"context":{"prompt":"review","inputArtifactIDs":["art_1"]}}`,
		"top-level inputFiles":     `{"prompt":"review","inputFiles":["a.md"]}`,
		"inputExtractions":         `{"context":{"inputExtractions":[{"text":"…"}]}}`,
		"broker inputs":            `{"inputs":{"doc":"…"}}`,
		"payload not an object":    `"just a string"`,
		// review a8a5 F2: a numeric or boolean value is not "empty".
		"numeric input count": `{"inputFiles":0}`,
		"boolean input flag":  `{"context":{"inputExtractions":false}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDistillFixture(t)
			w := f.wireTaint()
			w.step(t, "t1", "file_read")
			task := ordinaryTask()
			task.Payload = []byte(payload)
			f.assertRefused(t, f.runOrdinary(t, w, task), "task_inputs")
		})
	}
}

func TestDistiller_EmptyInputKeysDoNotRefuse(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	task := ordinaryTask()
	task.Payload = []byte(`{"prompt":"x","context":{"inputArtifactIDs":[],"inputFiles":null}}`)
	if n := f.runOrdinary(t, w, task); n != 1 {
		t.Fatalf("empty input keys declare nothing; got %d drafts", n)
	}
}

// --- unreadable record: fail closed ---

func TestDistiller_UnreadableRecordIsNotDistilled(t *testing.T) {
	cases := map[string]func(*distillFixture, *taintWiring){
		"no outcome repository":   func(f *distillFixture, _ *taintWiring) { f.e.outcomeRepo = nil },
		"no task lister":          func(f *distillFixture, _ *taintWiring) { f.e.persistTaskRepo = nil },
		"no execution repository": func(f *distillFixture, _ *taintWiring) { f.e.execRepo = nil },
		"step read fails":         func(_ *distillFixture, w *taintWiring) { w.outcomes.listErr = errors.New("down") },
		"execution read fails":    func(_ *distillFixture, w *taintWiring) { w.execs.err = errors.New("down") },
		"lineage walk fails":      func(_ *distillFixture, w *taintWiring) { w.lister.err = errors.New("down") },
		"child read fails": func(f *distillFixture, _ *taintWiring) {
			f.e.taskRepo = failingChildren{f.tasks}
		},
		"row of another task": func(_ *distillFixture, w *taintWiring) { w.outcomes.foreign = true },
		// review a8a5 minor: coverage is keyed on (execution, step).
		"row without a step id": func(_ *distillFixture, w *taintWiring) { w.outcomes.rows["t1"][0].StepID = "" },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDistillFixture(t)
			w := f.wireTaint()
			w.step(t, "t1", "file_read")
			breakIt(f, w)
			f.assertRefused(t, f.runOrdinary(t, w, ordinaryTask()), "taint_unreadable")
		})
	}
}

// review a8a5 F11: the proof is about the re-read row, so the model is fed
// from that row, never from the caller's copy of the task.
func TestDistiller_ModelSeesTheProvenRowNotTheCallersCopy(t *testing.T) {
	f := newDistillFixture(t)
	var seen string
	f.e.distillerLLM = recordingDistillLLM{content: distillVerdict, user: &seen}
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	stored := ordinaryTask()
	stored.Payload = []byte(`{"prompt":"the stored goal"}`)
	f.tasks.tasks[stored.ID] = stored
	w.lister.byID[stored.ID] = stored
	callers := ordinaryTask()
	callers.Payload = []byte(`{"prompt":"a goal nobody proved"}`)
	f.e.maybeDistillSkill(context.Background(), callers, "result text")
	if !strings.Contains(seen, "the stored goal") || strings.Contains(seen, "nobody proved") {
		t.Fatalf("the model must see the proven row's goal; got %q", seen)
	}
}

// recordingDistillLLM records the user message the distiller sent.
type recordingDistillLLM struct {
	chat.Provider
	content string
	user    *string
}

func (r recordingDistillLLM) Complete(ctx context.Context, m []chat.Message) (*chat.ChatResponse, error) {
	for _, msg := range m {
		if msg.Role == "user" {
			*r.user = msg.Content
		}
	}
	return fakeDistillLLM{content: r.content}.Complete(ctx, m)
}
