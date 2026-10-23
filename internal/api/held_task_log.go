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
	"vornik.io/vornik/internal/report"
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
// Caller precondition (review-20261008-1762 finding 2): with Scrub nil it
// renders the OPERATOR view, unscrubbed, which only an admin-class caller
// reads (GitHub #440, 2026-10-21: the logs and explain routes and the UI log
// panel serve ScrubbedView to every other caller, and the post-mortem
// explainer reads only ScrubbedView, #439). Before #440 this paragraph said
// project-scoped keys read the operator view too. Every caller outside that trust boundary
// reads the hub through a projection instead (live design, Change 2026-10-12,
// L0, the reader table): companion, broker and agent-admin keys through
// projectLiveEvent (the live socket, task_logs; task_explain reads no hub
// events at all, its renderer copy has a nil log fetcher), and an external A2A peer through livepubsub.ProjectLifecycle
// (the A2A SSE bridge). None of them is wired to this type.
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
	// Scrub, when set, makes TaskLogs return the SCRUBBED view: each hub
	// line rendered whole, passed through the scrubber, and only then capped
	// (redact before truncate), and the persisted excerpt scrubbed line by
	// line; a withheld line reads report.WithheldLine. Set it through
	// ScrubbedView, which refuses a nil scrubber, so asking for the scrubbed
	// view can never yield the operator view (process-spawn-law design,
	// "Amendment 2026-10-21 (#439, #440)", C2). Nil is the operator view.
	Scrub report.LineScrubber
}

// errNoLogScrubber is returned when the scrubbed view is asked for and no
// scrubber could be built.
var errNoLogScrubber = errors.New("the log scrubber could not be built")

// ScrubbedView returns h reading the scrubbed view with scrub. A nil scrub is
// an error: the caller then serves nothing rather than the operator view.
func (h HeldTaskLog) ScrubbedView(scrub report.LineScrubber) (HeldTaskLog, error) {
	if scrub == nil {
		return HeldTaskLog{}, errNoLogScrubber
	}
	h.Scrub = scrub
	return h, nil
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
		excerpt = strings.TrimRight(excerpt, "\n")
		if h.Scrub != nil {
			excerpt, _ = report.ScrubLines(excerpt, h.Scrub)
		}
		lines = append(lines, strings.Split(excerpt, "\n")...)
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
		if h.Scrub == nil {
			if line, ok := heldLogLine(evt, h.Uncapped); ok {
				out = append(out, line)
			}
			continue
		}
		// Redact before truncate: scrub every string of the payload as it is
		// (JSON escaping would put \n, \t or < before a key and defeat
		// the \b-anchored nets: adversarial review 2026-10-21, W16-F finding
		// 1), then render whole, scrub the line, then cap.
		if evt.Kind == livepubsub.KindLLMToken {
			continue // skipped in both views
		}
		if evt.Payload != nil {
			m, okMap := payloadAsMap(evt.Payload)
			if !okMap || scrubStrings(m, h.Scrub) {
				out = append(out, report.WithheldLine)
				continue
			}
			evt = withPayload(evt, m)
		}
		line, ok := heldLogLine(evt, true)
		if !ok {
			continue
		}
		line, _ = report.ScrubLines(line, h.Scrub)
		if !h.Uncapped {
			line = capHeldLine(line)
		}
		out = append(out, line)
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

// capHeldLine bounds a scrubbed line as heldLogLine bounds an operator one:
// the "<time> <kind> " prefix plus heldLogPayloadCap bytes of payload.
func capHeldLine(line string) string {
	const prefixAllowance = 64 // RFC3339 time + the longest kind name
	if len(line) <= heldLogPayloadCap+prefixAllowance {
		return line
	}
	return truncateUTF8(line, heldLogPayloadCap+prefixAllowance) + "…"
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
