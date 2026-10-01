package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// renderLiveTaskBody renders the live page for a running task.
func renderLiveTaskBody(t *testing.T) string {
	t.Helper()
	srv, taskRepo, execRepo := liveTaskServer(t)
	taskRepo.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
	}
	currentStep := "research"
	execRepo.ListFunc = func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return []*persistence.Execution{{
			ID: "exec_g1", TaskID: "task_g1", ProjectID: "p",
			Status: persistence.ExecutionStatusRunning, CurrentStepID: &currentStep,
		}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_g1/live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "task_g1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	return rr.Body.String()
}

// jsFunctionSource returns the full source of `function name(...) {...}` from
// body by brace matching, or "" when absent.
func jsFunctionSource(body, name string) string {
	start := strings.Index(body, "function "+name+"(")
	if start < 0 {
		return ""
	}
	open := strings.Index(body[start:], "{")
	if open < 0 {
		return ""
	}
	depth := 0
	for i := start + open; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[start : i+1]
			}
		}
	}
	return ""
}

// TestLiveTask_StatusHandlersRefuseStaleSeq is the page half of the
// 2026-09-28 T-0d3c fix (exec_20260928151318_1c35d6b2b9cedb83): a DB replay
// fanned historical step_started frames out to already-open pages, and
// onStepStarted set the badge to "running" unconditionally, so finished steps
// (schema_violation in execution_step_outcomes) showed RUNNING. Every
// badge-setting handler must now pass the frame's seq through applyStatusSeq
// BEFORE touching the badge, and applyStatusSeq must refuse a seq that is not
// newer than the one that last set the card's status.
func TestLiveTask_StatusHandlersRefuseStaleSeq(t *testing.T) {
	body := renderLiveTaskBody(t)

	guard := jsFunctionSource(body, "applyStatusSeq")
	if guard == "" {
		t.Fatal("applyStatusSeq(card, seq) missing from task_live.html — a replayed step_started can regress a finished card to running")
	}
	for _, h := range []string{"onStepStarted", "onStepCompleted", "onOutcomeRecorded"} {
		src := jsFunctionSource(body, h)
		if src == "" {
			t.Fatalf("%s missing from the page", h)
		}
		g := strings.Index(src, "applyStatusSeq(")
		b := strings.Index(src, "card.badge.")
		if g < 0 || b < 0 || g > b {
			t.Errorf("%s must refuse a stale seq via applyStatusSeq before it touches card.badge:\n%s", h, src)
		}
	}
	dispatch := jsFunctionSource(body, "handleEventFrame")
	for _, c := range []string{"onStepStarted(payload, evt.seq)", "onStepCompleted(payload, evt.seq)", "onOutcomeRecorded(payload, evt.seq)"} {
		if !strings.Contains(dispatch, c) {
			t.Errorf("handleEventFrame must pass the frame seq: %q missing", c)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the applyStatusSeq behaviour check needs it (the structural checks above ran)")
	}
	// Run the page's REAL handlers against a stub DOM: the sequence below
	// is the T-0d3c incident — a step finishes, then a replayed (older)
	// step_started arrives.
	script := guard + "\n" +
		jsFunctionSource(body, "onStepStarted") + "\n" +
		jsFunctionSource(body, "onStepCompleted") + "\n" +
		jsFunctionSource(body, "onOutcomeRecorded") + "\n" + `
const stepCards = {};
let active = '';
function ensureStepCard(id) {
  if (!stepCards[id]) stepCards[id] = {badge: {}, details: {}};
  return stepCards[id];
}
function setActiveStep(id) { active = id; }
const assert = (cond, msg) => { if (!cond) { console.error(msg); process.exit(1); } };
const badge = (id) => stepCards[id].badge.textContent;

// applyStatusSeq itself.
const c = {};
assert(applyStatusSeq(c, 10) === true, 'first status event must apply');
assert(applyStatusSeq(c, 5) === false, 'older seq must be refused');
assert(applyStatusSeq(c, 10) === false, 'duplicate seq must be refused');
assert(applyStatusSeq(c, 11) === true, 'newer seq must apply');
assert(applyStatusSeq(c, undefined) === true, 'a frame without a seq is never refused');
assert(applyStatusSeq(c, -1) === true, 'a synthetic seq -1 frame is never refused');
assert(applyStatusSeq(c, 9) === false, 'unsequenced frames must not lower the high-water mark');

// The incident, through the handlers.
onStepStarted({step_id: 'research'}, 5);
assert(badge('research') === 'running', 'a fresh step_started must show running, got ' + badge('research'));
onOutcomeRecorded({step_id: 'research', class: 'schema_violation'}, 9);
assert(badge('research') === 'schema_violation', 'outcome must apply, got ' + badge('research'));
onStepStarted({step_id: 'research_infra_retry3'}, 10);
onStepCompleted({step_id: 'research_infra_retry3', outcome: 'schema_violation'}, 14);
active = 'other';
onStepStarted({step_id: 'research'}, 5);
onStepStarted({step_id: 'research_infra_retry3'}, 10);
assert(badge('research') === 'schema_violation', 'replayed step_started regressed research to ' + badge('research'));
assert(badge('research_infra_retry3') === 'schema_violation', 'replayed step_started regressed the retry to ' + badge('research_infra_retry3'));
assert(active === 'other', 'a stale step_started must not change the active step, got ' + active);
onOutcomeRecorded({step_id: 'research', class: 'ok'}, 9);
assert(badge('research') === 'schema_violation', 'a duplicate outcome frame must be refused');
// A genuinely newer start of the same step still applies.
onStepStarted({step_id: 'research'}, 20);
assert(badge('research') === 'running' && active === 'research', 'a newer step_started must apply');
console.log('ok');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("applyStatusSeq behaviour check failed: %v\n%s", err, out)
	}
}
