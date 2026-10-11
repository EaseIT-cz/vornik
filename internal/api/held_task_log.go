package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// HeldTaskLog is the one task log a request may read: state the daemon
// already holds, never a process (GitHub #294; process-spawn-law design,
// "Change 2026-10-08 — no request reads container logs"). It serves
// GET .../tasks/{id}/logs, the explain renderer, the support report, the UI
// log panel and the UI post-mortem. Its TaskLogs method has the signature of
// api's, ui's and postmortem's log interfaces, so it is wired to each.
//
// It composes the two sources the #92 agent tools read: the live hub replay
// of the task's newest execution, in the operator view, and the persisted
// excerpt (the execution's error, which carries the container-log tail the
// executor captured when a step failed; else the result message; else the
// task's last_error). Nil fields degrade to whatever the others hold.
//
// Caller precondition (review-20261008-1762 finding 2): it renders the
// OPERATOR view, unscrubbed. Wire it only to routes that admit operator and
// project-scoped keys, which already read the same view on
// GET /api/v1/executions/{id}/live. Companion, broker and agent-admin keys
// read through task_logs and task_explain, which project and scrub.
type HeldTaskLog struct {
	Tasks      persistence.TaskRepository
	Executions persistence.ExecutionRepository
	Live       LiveSubscriber
	// Uncapped renders every event payload in full. The support report sets
	// it, so its redaction runs over whole values: a payload cut at the cap
	// could leave a secret fragment the detector no longer matches
	// (support-report design §6, redact-before-truncate; review-20261008-3356
	// finding 3). The UI and CLI keep the cap for readability; they are not
	// redacted.
	Uncapped bool
}

const (
	// heldLogPersistedMarker separates the hub lines from the persisted
	// excerpt. It is written only when both are present.
	heldLogPersistedMarker = "--- persisted ---"
	// heldLogPayloadCap bounds one event line's payload, so a single tool
	// output cannot fill the window.
	heldLogPayloadCap = 512
)

var errNoHeldTaskLog = errors.New("no logs available for task")

var _ TaskLogSource = HeldTaskLog{}

// TaskLogs returns at most tail lines (all when tail <= 0), newest last.
func (h HeldTaskLog) TaskLogs(ctx context.Context, taskID string, tail int) (string, error) {
	if strings.TrimSpace(taskID) == "" {
		return "", errors.New("task_id is required")
	}
	// The task's project is the authority every row read must match; without
	// a task repository the execution's own project stands in.
	projectID, known := "", false
	if h.Tasks != nil {
		task, err := h.Tasks.Get(ctx, taskID)
		if err != nil || task == nil {
			return "", errNoHeldTaskLog
		}
		projectID, known = task.ProjectID, true
	}
	var lines []string
	if h.Executions != nil {
		exec, err := h.Executions.GetByTaskID(ctx, taskID)
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			return "", fmt.Errorf("lookup execution: %w", err)
		}
		if exec != nil && (!known || exec.ProjectID == projectID) {
			projectID, known = exec.ProjectID, true
			lines = h.hubLines(exec.ID)
		}
	}
	excerpt := ""
	if known {
		excerpt, _ = persistedTaskLogExcerpt(ctx, h.Tasks, h.Executions, projectID, taskID)
	}
	if strings.TrimSpace(excerpt) != "" {
		if len(lines) > 0 {
			lines = append(lines, heldLogPersistedMarker)
		}
		lines = append(lines, strings.Split(strings.TrimRight(excerpt, "\n"), "\n")...)
	}
	if len(lines) == 0 {
		return "", errNoHeldTaskLog
	}
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return strings.Join(lines, "\n"), nil
}

// hubLines drains the execution's replay without waiting: the subscription
// delivers its history before Subscribe returns.
func (h HeldTaskLog) hubLines(executionID string) []string {
	if h.Live == nil {
		return nil
	}
	ch, cancel, err := h.Live.Subscribe(executionID, 0)
	if err != nil {
		return nil
	}
	defer cancel()
	var out []string
	for _, evt := range drainLive(ch, nil) {
		if line, ok := heldLogLine(evt, h.Uncapped); ok {
			out = append(out, line)
		}
	}
	return out
}

// heldLogLine renders one event in the operator view (bytes unchanged):
// "<time> <kind> <payload as compact JSON, capped unless uncapped>".
// llm_token is skipped: there is one per token, and llm_call_finished carries
// the call.
func heldLogLine(evt livepubsub.LiveEvent, uncapped bool) (string, bool) {
	evt, keep := projectLiveEvent(evt, liveViewOperator, nil, nil)
	if !keep || evt.Kind == livepubsub.KindLLMToken {
		return "", false
	}
	ts := evt.Timestamp.UTC().Format(time.RFC3339)
	if evt.Kind == livepubsub.KindReplayGap {
		oldest := ""
		if m, ok := payloadAsMap(evt.Payload); ok {
			if v, ok := m["oldest_seq"].(float64); ok {
				oldest = fmt.Sprintf(" (oldest seq %d)", int64(v))
			}
		}
		return ts + " earlier events are no longer held" + oldest, true
	}
	payload := ""
	if evt.Payload != nil {
		if b, err := json.Marshal(evt.Payload); err == nil {
			payload = string(b)
		}
	}
	if !uncapped && len(payload) > heldLogPayloadCap {
		payload = truncateUTF8(payload, heldLogPayloadCap) + "…"
	}
	return strings.TrimRight(ts+" "+evt.Kind+" "+payload, " "), true
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
