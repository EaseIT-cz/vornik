package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/report"
)

// task_logs: an agentic client reads and follows its own task's live events
// (live-task-observation design, Change 2026-10-08 C4, GitHub #92). A
// cursor-paged long poll (operator decision O1): every call ends at its wait
// and byte budget; there is no open stream on the MCP endpoint.

const (
	taskLogsMaxBytes        = 64 * 1024
	taskLogsDefaultTail     = 50
	taskLogsMaxTail         = 500
	taskLogsDrainAfterFirst = 250 * time.Millisecond
	// taskLogsMaxWaitSeconds bounds one task_logs long poll. Its own constant,
	// equal to result's today but not tied to it (review-20261008-f680 F6).
	taskLogsMaxWaitSeconds = 25
	// Global caps across keys, so many keys cannot together hold unbounded
	// waits or streams (review-20261008-f680 F4).
	liveAgentStreamsGlobal = 64
	logWaitersGlobal       = 64
	// logBucketIdleTTL: a bucket idle this long has refilled completely and is
	// equivalent to none, so it is dropped (review-20261008-f680 F4).
	logBucketIdleTTL = time.Minute
	// logBucketsSweepAt forces a sweep of idle buckets once the map is this
	// large; logBucketsHardCap is its hard bound: a new key past it is refused
	// until buckets go idle (review-20261008-a4ef F4).
	logBucketsSweepAt = 1024
	logBucketsHardCap = 4096
	// companionLogToolsPerMinute is the per-key budget shared by the four log
	// and diagnosis tools (task_logs, daemon_log, task_explain, doctor). It
	// does not cover status, result or cancel (review-20261008-fb6c F3).
	companionLogToolsPerMinute = 30
)

// liveLogState is the per-server state of the agent log path, built on first
// use: the shared scrubber, the per-key rate limiter, the per-key wait and
// stream counters, and the test seams for the clock and the wait timer.
type liveLogState struct {
	scrub report.LineScrubber // nil when the detector cannot be built: no companion view

	mu              sync.Mutex
	buckets         map[string]*logBucket
	lastSweep       time.Time
	lastForcedSweep time.Time
	waiters         map[string]int
	streams         map[string]int
	waitersTotal    int
	streamsTotal    int
	maxWaitersTotal int
	maxStreamsTotal int
	now             func() time.Time
	newTimer        func(time.Duration) (<-chan time.Time, func() bool)
}

type logBucket struct {
	tokens float64
	last   time.Time
}

func newLiveLogState() *liveLogState {
	st := &liveLogState{
		buckets:         map[string]*logBucket{},
		waiters:         map[string]int{},
		streams:         map[string]int{},
		maxWaitersTotal: logWaitersGlobal,
		maxStreamsTotal: liveAgentStreamsGlobal,
		now:             time.Now,
		newTimer: func(d time.Duration) (<-chan time.Time, func() bool) {
			t := time.NewTimer(d)
			return t.C, t.Stop
		},
	}
	host, _ := os.Hostname()
	if scrub, err := report.NewLineScrubber(host); err == nil {
		st.scrub = scrub
	}
	return st
}

// liveLogs returns the server's log-path state, building it once.
func (s *Server) liveLogs() *liveLogState {
	s.liveLogsOnce.Do(func() {
		if s.liveLogsState == nil {
			s.liveLogsState = newLiveLogState()
		}
	})
	return s.liveLogsState
}

// allow takes one token from key's bucket (30 per minute, refilled
// continuously). It returns the wait until the next token when refused.
func (st *liveLogState) allow(keyID string) (bool, time.Duration) {
	return st.allowN(keyID, 1)
}

// allowN takes cost tokens from key's bucket. The bucket map is bounded
// (review-20261008-a4ef F4): past logBucketsSweepAt a new key forces a sweep
// of idle buckets, and past logBucketsHardCap a new key is refused until
// buckets go idle.
func (st *liveLogState) allowN(keyID string, cost int) (bool, time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	st.sweepBucketsLocked(now)
	b := st.buckets[keyID]
	if b == nil {
		if len(st.buckets) >= logBucketsSweepAt && now.Sub(st.lastForcedSweep) >= time.Second {
			// At most one forced sweep a second, so filling the map is not a
			// sweep per new key (review-20261008-f42d F6).
			st.lastForcedSweep = now
			st.lastSweep = time.Time{}
			st.sweepBucketsLocked(now)
		}
		if len(st.buckets) >= logBucketsHardCap {
			return false, logBucketIdleTTL
		}
		b = &logBucket{tokens: companionLogToolsPerMinute, last: now}
		st.buckets[keyID] = b
	}
	rate := float64(companionLogToolsPerMinute) / 60.0 // tokens per second
	b.tokens = math.Min(companionLogToolsPerMinute, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= float64(cost) {
		b.tokens -= float64(cost)
		return true, 0
	}
	return false, time.Duration((float64(cost) - b.tokens) / rate * float64(time.Second))
}

// sweepBucketsLocked drops buckets idle for logBucketIdleTTL (full again, so
// equivalent to absent). It runs at most once per TTL. Caller holds mu.
func (st *liveLogState) sweepBucketsLocked(now time.Time) {
	if now.Sub(st.lastSweep) < logBucketIdleTTL {
		return
	}
	st.lastSweep = now
	for k, b := range st.buckets {
		if now.Sub(b.last) >= logBucketIdleTTL {
			delete(st.buckets, k)
		}
	}
}

// acquireWaiter reserves one of the key's concurrent long-poll slots, within
// the global cap.
func (st *liveLogState) acquireWaiter(keyID string) (func(), bool) {
	return st.acquire(st.waiters, &st.waitersTotal, st.maxWaitersTotal, companionWaitersPerKey, keyID)
}

// acquireStream reserves one of the key's concurrent live-socket slots (C1),
// within the global cap.
func (st *liveLogState) acquireStream(keyID string) (func(), bool) {
	return st.acquire(st.streams, &st.streamsTotal, st.maxStreamsTotal, liveAgentStreamsPerKey, keyID)
}

// acquire takes a slot from a per-key counter and its global total. The
// release removes the key's entry when it reaches zero, so the map holds only
// keys with something open (review-20261008-f680 F4).
func (st *liveLogState) acquire(counts map[string]int, total *int, maxTotal, perKey int, keyID string) (func(), bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if counts[keyID] >= perKey || *total >= maxTotal {
		return nil, false
	}
	counts[keyID]++
	*total++
	var once sync.Once
	return func() {
		once.Do(func() {
			st.mu.Lock()
			defer st.mu.Unlock()
			*total--
			if counts[keyID] <= 1 {
				delete(counts, keyID)
				return
			}
			counts[keyID]--
		})
	}, true
}

// gateLogTool applies the shared per-key rate limit; the error names the wait.
func (s *Server) gateLogTool(key *persistence.APIKey, tool string) error {
	cost := 1
	if tool == "doctor" {
		cost = doctorRateCost
	}
	if ok, wait := s.liveLogs().allowN(key.ID, cost); !ok {
		recordLogToolOutcome(tool, "rate_limited")
		return fmt.Errorf("RATE_LIMITED: %s and the other log tools allow %d calls per minute per key; retry in %d s",
			tool, companionLogToolsPerMinute, int(math.Ceil(wait.Seconds())))
	}
	return nil
}

type taskLogsArgs struct {
	TaskID      string `json:"task_id"`
	Cursor      string `json:"cursor"`
	Tail        int    `json:"tail"`
	WaitSeconds int    `json:"wait_seconds"`
}

// parseLogCursor splits "<execution_id>:<seq>" at the LAST colon, so the
// grammar stays unambiguous even for an id containing one (generated ids,
// exec_<time>_<hex>, have none). Clients must not parse it; the server
// re-validates it on every call.
func parseLogCursor(c string) (string, int64, error) {
	i := strings.LastIndex(c, ":")
	if i <= 0 || i == len(c)-1 {
		return "", 0, errors.New("invalid cursor")
	}
	seq, err := strconv.ParseInt(c[i+1:], 10, 64)
	if err != nil || seq < 0 {
		return "", 0, errors.New("invalid cursor")
	}
	return c[:i], seq, nil
}

func formatLogCursor(execID string, seq int64) string {
	return execID + ":" + strconv.FormatInt(seq, 10)
}

func (s *Server) companionToolTaskLogs(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if s.taskRepo == nil {
		return "", errors.New("task repo not wired")
	}
	var args taskLogsArgs
	if err := decodeToolArgs("task_logs", raw, &args); err != nil {
		return "", err
	}
	args.TaskID = strings.TrimSpace(args.TaskID)
	if args.TaskID == "" {
		return "", errors.New("task_id is required")
	}
	// Rate limit first, and any lookup failure answers as an unknown task
	// (review-20261008-3e8e F1, F2).
	if err := s.gateLogTool(key, "task_logs"); err != nil {
		return "", err
	}
	task, err := s.taskRepo.Get(ctx, args.TaskID)
	if err != nil || task == nil || !keyCoversProject(key, task.ProjectID) {
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			s.logger.Warn().Err(err).Str("task_id", args.TaskID).Msg("task_logs: task lookup failed")
		}
		recordLogToolOutcome("task_logs", "not_found")
		return "", fmt.Errorf("task %s not found", args.TaskID)
	}
	view := s.liveViewFor(key, task)
	st := s.liveLogs()
	if view == liveViewCompanion && st.scrub == nil {
		recordLogToolOutcome("task_logs", "refused")
		return "", errors.New("task_logs is unavailable: the log scrubber could not be built, so no log is served")
	}
	page, err := s.readTaskLogPage(ctx, key, task, args, view, st)
	if err != nil {
		return "", err
	}
	recordLogToolOutcome("task_logs", "ok")
	b, _ := json.MarshalIndent(page, "", "  ")
	return string(b), nil
}

// taskLogPage is task_logs' answer.
type taskLogPage struct {
	TaskID           string                 `json:"task_id"`
	ExecutionID      string                 `json:"execution_id"`
	Attempt          int                    `json:"attempt"`
	View             string                 `json:"view"`
	Events           []livepubsub.LiveEvent `json:"events"`
	NextCursor       string                 `json:"next_cursor"`
	ReplayGap        map[string]int64       `json:"replay_gap,omitempty"`
	ExecutionChanged bool                   `json:"execution_changed,omitempty"`
	Complete         bool                   `json:"complete"`
	LogTail          []string               `json:"log_tail,omitempty"`
	WaitCapped       bool                   `json:"wait_capped,omitempty"`
}

func viewName(v liveView) string {
	switch v {
	case liveViewBroker:
		return "broker"
	case liveViewCompanion:
		return "companion"
	case liveViewOperator:
		return "operator"
	default:
		// Never served: projectLiveEvent drops everything for an unknown view.
		return "unknown"
	}
}

func (s *Server) readTaskLogPage(ctx context.Context, key *persistence.APIKey, task *persistence.Task, args taskLogsArgs, view liveView, st *liveLogState) (*taskLogPage, error) {
	page := &taskLogPage{TaskID: task.ID, Attempt: task.Attempt, View: viewName(view), Events: []livepubsub.LiveEvent{}}
	terminal := isTerminalStatus(task.Status)
	exec, err := s.newestExecution(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if exec == nil {
		page.Complete = terminal
		s.addLogTail(ctx, page, task, view, st)
		return page, nil
	}
	page.ExecutionID = exec.ID
	fromSeq, tailMode, err := resolveLogCursor(page, exec.ID, args.Cursor)
	if err != nil {
		return nil, err
	}
	if s.liveSub == nil {
		return nil, errors.New("live observation is not wired on this daemon")
	}
	ch, cancel, err := s.liveSub.Subscribe(exec.ID, fromSeq)
	if err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	defer cancel()

	raw := drainLive(ch, nil)
	if len(raw) == 0 && args.WaitSeconds > 0 && !terminal {
		if release, ok := st.acquireWaiter(key.ID); ok {
			raw = waitLive(ctx, ch, st, args.WaitSeconds)
			release()
		} else {
			page.WaitCapped = true
			recordLogToolOutcome("task_logs", "wait_capped")
		}
	}
	events, gapOldest := splitReplayGap(raw)
	if tailMode {
		if tail := clampTail(args.Tail); len(events) > tail {
			events = events[len(events)-tail:]
		}
	} else if gapOldest >= 0 {
		page.ReplayGap = map[string]int64{"oldest_seq": gapOldest}
	}
	truncated := fillLogPage(page, exec.ID, events, view, st.scrub, func(p string) bool { return keyCoversProject(key, p) })
	page.Complete = terminal && !truncated
	if page.Complete {
		s.addLogTail(ctx, page, task, view, st)
	}
	return page, nil
}

// newestExecution is the task's newest execution (one per attempt), or nil.
func (s *Server) newestExecution(ctx context.Context, taskID string) (*persistence.Execution, error) {
	if s.executionRepo == nil {
		return nil, nil
	}
	e, err := s.executionRepo.GetByTaskID(ctx, taskID)
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return nil, fmt.Errorf("lookup execution: %w", err)
	}
	return e, nil
}

// resolveLogCursor sets the page's starting cursor and returns where to
// subscribe from. No cursor is tail mode. A cursor naming an older execution
// restarts at the newest one from its start (execution_changed).
func resolveLogCursor(page *taskLogPage, execID, cursor string) (int64, bool, error) {
	page.NextCursor = cursor
	if cursor == "" {
		return 0, true, nil
	}
	curExec, seq, err := parseLogCursor(cursor)
	if err != nil {
		return 0, false, err
	}
	if curExec == execID {
		return seq + 1, false, nil
	}
	page.ExecutionChanged = true
	page.NextCursor = ""
	return 0, false, nil
}

func clampTail(tail int) int {
	if tail <= 0 {
		return taskLogsDefaultTail
	}
	if tail > taskLogsMaxTail {
		return taskLogsMaxTail
	}
	return tail
}

// splitReplayGap removes the publisher's replay_gap marker and returns its
// oldest seq (-1 when there was none).
func splitReplayGap(raw []livepubsub.LiveEvent) ([]livepubsub.LiveEvent, int64) {
	var gapOldest int64 = -1
	events := make([]livepubsub.LiveEvent, 0, len(raw))
	for _, evt := range raw {
		if evt.Kind != livepubsub.KindReplayGap {
			events = append(events, evt)
			continue
		}
		if m, ok := payloadAsMap(evt.Payload); ok {
			if v, ok := m["oldest_seq"].(float64); ok {
				gapOldest = int64(v)
			}
		}
	}
	return events, gapOldest
}

// fillLogPage projects events into the page up to the byte budget, cut at an
// event boundary. Every examined event advances the cursor, including one the
// view drops. A single event over the budget becomes an oversize marker so the
// cursor still moves. It reports whether the budget cut the page short.
func fillLogPage(page *taskLogPage, execID string, events []livepubsub.LiveEvent, view liveView, scrub report.LineScrubber, covers func(string) bool) bool {
	used, withheld := 0, 0
	defer func() {
		if withheld > 0 {
			recordLogWithheld("task_logs", withheld)
		}
	}()
	for _, evt := range events {
		out, keep := projectLiveEvent(evt, view, scrub, covers)
		if keep {
			b, _ := json.Marshal(out)
			if used+len(b) > taskLogsMaxBytes {
				if len(page.Events) > 0 {
					return true
				}
				// The marker is about 120 bytes (ids, seq, time, kind), far under
				// the budget, so it always fits an empty page.
				out = livepubsub.LiveEvent{ExecutionID: evt.ExecutionID, Seq: evt.Seq, Timestamp: evt.Timestamp, Kind: livepubsub.KindOversize}
				b, _ = json.Marshal(out)
			}
			if out.Kind == livepubsub.KindWithheld {
				withheld++
			}
			used += len(b)
			page.Events = append(page.Events, out)
		}
		page.NextCursor = formatLogCursor(execID, evt.Seq)
	}
	return false
}

// drainLive takes every event already queued on ch without blocking. The
// subscription delivers its replay before Subscribe returns, so this is the
// history plus whatever is live at this moment.
func drainLive(ch <-chan livepubsub.LiveEvent, into []livepubsub.LiveEvent) []livepubsub.LiveEvent {
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return into
			}
			into = append(into, evt)
		default:
			return into
		}
	}
}

// waitLive blocks for the first event up to seconds (clamped to 25), then
// drains for at most taskLogsDrainAfterFirst more.
func waitLive(ctx context.Context, ch <-chan livepubsub.LiveEvent, st *liveLogState, seconds int) []livepubsub.LiveEvent {
	if seconds > taskLogsMaxWaitSeconds {
		seconds = taskLogsMaxWaitSeconds
	}
	deadline, stop := st.newTimer(time.Duration(seconds) * time.Second)
	defer stop()
	var out []livepubsub.LiveEvent
	select {
	case evt, ok := <-ch:
		if !ok {
			return out
		}
		out = append(out, evt)
	case <-deadline:
		return out
	case <-ctx.Done():
		return out
	}
	settle, stopSettle := st.newTimer(taskLogsDrainAfterFirst)
	defer stopSettle()
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, evt)
		case <-settle:
			return out
		case <-ctx.Done():
			return out
		}
	}
}

// addLogTail adds the persisted log tail for the companion view: the stored
// execution error, result message or last_error, line by line through the
// scrubber. It reads no process (A0.8's podman logs path was removed with
// GitHub #294). The broker view has no log tail.
func (s *Server) addLogTail(ctx context.Context, page *taskLogPage, task *persistence.Task, view liveView, st *liveLogState) {
	if view != liveViewCompanion || st.scrub == nil || !page.Complete {
		return
	}
	text, err := s.persistedTaskLogExcerpt(ctx, task.ProjectID, task.ID)
	if err != nil || strings.TrimSpace(text) == "" {
		return
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > taskLogsDefaultTail {
		lines = lines[len(lines)-taskLogsDefaultTail:]
	}
	withheld := 0
	for _, l := range lines {
		out, w := st.scrub(l)
		if w {
			out = "[withheld: secret]"
			withheld++
		}
		page.LogTail = append(page.LogTail, out)
	}
	if withheld > 0 {
		recordLogWithheld("task_logs", withheld)
	}
}
