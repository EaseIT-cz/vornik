package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/authsession"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/report"
)

// liveClientHello is the first frame the client sends on a
// successful upgrade. Carries the seq cursor for replay. A
// last_seq of 0 means "send the full ring buffer".
type liveClientHello struct {
	LastSeq int64 `json:"last_seq"`
}

const (
	// liveWriteTimeout caps each write to the client so a wedged
	// reader can't backpressure the publisher's fan-out.
	liveWriteTimeout = 5 * time.Second
	// livePingInterval is how often the server pings the client
	// to detect a half-open connection. A ping unanswered within
	// another interval closes the connection (about 30s from opening
	// when the first ping fails).
	livePingInterval = 15 * time.Second
	// liveReadLimit caps the maximum WebSocket frame size we
	// read from the client. The hello frame carries a single
	// JSON int — 4 KiB is generous. Without a limit a hostile
	// client could send a multi-GB frame and OOM the daemon
	// before the library returns from Read.
	liveReadLimit = 4 * 1024

	// Bounds for a companion or agent-admin key's stream (live-task-observation
	// design, Change 2026-10-08 C1). Operator streams keep the one-hour bound.
	liveAgentStreamsPerKey = 2
	liveAgentMaxDuration   = 15 * time.Minute
	liveAgentMaxBytes      = 4 << 20
)

// liveTerminalCheckEvery is how often a stream re-reads its execution to end
// at terminal status. It is the server's own interval, fixed at construction
// (WithLiveTerminalCheckInterval); no longer a package variable, which tests
// wrote while an earlier test's watcher still read it (GitHub #364).
func (s *Server) liveTerminalCheckEvery() time.Duration {
	if s.liveCheckEvery > 0 {
		return s.liveCheckEvery
	}
	return livePingInterval
}

// executionNotFound is the one answer for an unknown execution and, for an
// agent key, for an execution in a project the key does not cover (C1: no
// existence leak).
func executionNotFound(w http.ResponseWriter) {
	respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
}

func isTerminalExecution(st persistence.ExecutionStatus) bool {
	switch st {
	case persistence.ExecutionStatusCompleted, persistence.ExecutionStatusFailed, persistence.ExecutionStatusCancelled:
		return true
	}
	return false
}

// liveCaller is who is reading a stream and what they may see.
type liveCaller struct {
	key     *persistence.APIKey // nil for an operator caller
	rawKey  string              // the agent's bearer, re-checked on every tick
	view    liveView
	release func()
}

// resolveLiveCaller authorises the request for exec. ok is false when it
// already answered. An agent key (any key with a client kind) is scoped with
// keyCoversProject and misses answer exactly as an unknown id; an operator
// keeps requestAllowsProject and its 403.
func (s *Server) resolveLiveCaller(w http.ResponseWriter, r *http.Request, exec *persistence.Execution) (liveCaller, bool) {
	if APIKeyClientKindFromContext(r.Context()) == "" {
		if !requestAllowsProject(r, exec.ProjectID) {
			respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
			return liveCaller{}, false
		}
		return liveCaller{view: liveViewOperator, release: func() {}}, true
	}
	// Agent keys are not browsers (review-20261008-f680 F7): an agent request
	// that also carries a browser session cookie is refused, and the upgrade
	// below admits only the same origin or no Origin header at all.
	if _, cerr := r.Cookie(authsession.SessionCookieName); cerr == nil {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "an agent key does not ride a browser session")
		return liveCaller{}, false
	}
	key, err := s.resolveCompanionKey(r)
	if err != nil || !keyCoversProject(key, exec.ProjectID) {
		executionNotFound(w)
		return liveCaller{}, false
	}
	view := s.liveViewFor(key, s.liveTaskFor(r.Context(), exec))
	st := s.liveLogs()
	if view == liveViewCompanion && st.scrub == nil {
		respondError(w, http.StatusServiceUnavailable, "LIVE_UNAVAILABLE", "the log scrubber could not be built, so no log is served")
		return liveCaller{}, false
	}
	release, ok := st.acquireStream(key.ID)
	if !ok {
		respondError(w, http.StatusTooManyRequests, "RATE_LIMITED", "this key already holds its live streams; close one first")
		return liveCaller{}, false
	}
	return liveCaller{key: key, rawKey: APIKeyFromContext(r.Context()), view: view, release: release}, true
}

// ExecutionLive handles GET /api/v1/executions/{id}/live.
// Upgrades the connection to a WebSocket and streams LiveEvents
// for the execution until the client disconnects or the daemon
// shuts down.
//
// Auth: the existing API-key middleware has already validated
// the bearer key before the handler runs (or, when keys are
// disabled, anyone reaches this point). Project scope is
// checked against the execution's project_id before the
// upgrade.
//
// Connection lifecycle:
//  1. Validate execution + caller scope. 404 / 403 on failure;
//     socket is NOT opened so the operator gets a clean HTTP
//     response shape.
//  2. Upgrade. The handler accepts compressed frames + the
//     OriginPatterns wildcard since the daemon serves browsers
//     on the same host.
//  3. Read the client hello frame ({"last_seq": N}) with a
//     short deadline. Bad/absent hello falls back to
//     last_seq=0 (full replay).
//  4. Subscribe to the publisher; replay any buffered events
//     with seq >= last_seq+1; stream live thereafter.
//  5. Send a keepalive ping every livePingInterval. Drop the
//     connection when a ping goes unanswered or the writer
//     times out.
//  6. On any error or context cancel, close cleanly with a
//     normal-closure code.
func (s *Server) ExecutionLive(w http.ResponseWriter, r *http.Request, executionID string) {
	if s.liveSub == nil {
		respondError(w, http.StatusServiceUnavailable, "LIVE_DISABLED",
			"live observation not wired on this deployment")
		return
	}
	if s.executionRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Execution repository not available")
		return
	}

	// Auth + scope happen BEFORE the upgrade so a refusal lands as a
	// plain HTTP response. Once the socket's open we can only
	// communicate via close frames + status codes.
	exec, err := s.executionRepo.Get(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			executionNotFound(w)
			return
		}
		s.logger.Error().Err(err).Str("executionId", executionID).
			Msg("live: failed to load execution")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to load execution")
		return
	}
	if exec == nil {
		executionNotFound(w)
		return
	}
	caller, ok := s.resolveLiveCaller(w, r, exec)
	if !ok {
		return
	}
	defer caller.release()

	// The key rides a header (X-API-Key, Basic or Bearer) or the browser's
	// session cookie; no query-string key is read anywhere (a key in a URL
	// ends up in access logs), live design Change 2026-10-08 C1.
	//
	// Origin enforcement: the library's default behaviour authorises
	// the request host (same-origin browsers always pass). Reverse-
	// proxy deployments add their public host through
	// WithLiveAllowedOrigins so the daemon's own UI keeps working
	// behind nginx/Caddy/Cloudflare. Wildcards permitted (path.Match
	// semantics) for *.example.com style configs. Empty list ==
	// same-origin only, which is the secure default.
	// Origin policy (review-20261008-f680 F7): an operator's browser UI may be
	// served from the configured extra origins; an agent key gets none of
	// them, only the request's own host (or no Origin header: a non-browser).
	originPatterns := s.liveAllowedOrigins
	if caller.key != nil {
		originPatterns = nil
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns,
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("executionId", executionID).
			Str("origin", r.Header.Get("Origin")).
			Msg("live: websocket upgrade failed")
		return
	}
	// Cap the read side so a hostile client can't OOM the daemon
	// with an outsized hello frame. Applies to every subsequent
	// read on this connection too.
	conn.SetReadLimit(liveReadLimit)
	// Sentinel close on any return path. NormalClosure (1000) =
	// "we're done"; the JS client handles it as a clean close.
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	// Detached context so close-on-cancel works even after the
	// handler returns. Bounded by max stream duration (1h) to
	// guarantee resource release on operator browsers that get
	// left open indefinitely.
	lifetime := 1 * time.Hour
	if caller.key != nil {
		lifetime = liveAgentMaxDuration
	}
	ctx, cancel := context.WithTimeout(r.Context(), lifetime)
	defer cancel()

	// Wait briefly for the client hello. Absence or malformed JSON falls
	// back to the query cursor, then 0, and the stream continues (audit
	// 2026-10-12 finding B: the wait must not close the connection). The
	// returned context ends when the client disconnects or sends a second
	// application frame (issue #35); control frames keep being read so Ping
	// observes pongs.
	hello, readCtx, stopRead := startLiveReader(ctx, conn, parseLiveLastSeqFromQuery(r))
	defer stopRead()
	ctx = readCtx

	events, unsub, err := s.liveSub.Subscribe(executionID, liveSubscribeFromSeq(hello.LastSeq))
	if err != nil {
		s.logger.Warn().Err(err).Str("executionId", executionID).
			Msg("live: subscribe failed")
		_ = conn.Close(websocket.StatusInternalError, "subscribe failed")
		return
	}
	defer unsub()

	s.streamLive(ctx, conn, events, exec, caller)
}

// streamLive is the heartbeat and writer of one stream. Each event passes the
// caller's projection; an agent stream also ends at its byte budget and its
// lifetime; every stream ends once the execution is terminal and quiet.
func (s *Server) streamLive(ctx context.Context, conn *websocket.Conn, events <-chan livepubsub.LiveEvent, exec *persistence.Execution, caller liveCaller) {
	// Heartbeat goroutine — server pings the client; the library
	// auto-replies to client pings through the active control-frame reader.
	// One unanswered ping closes the connection after its 15s timeout.
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(livePingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				close(pingDone)
				return
			case <-ticker.C:
				pingCtx, pingCancel := context.WithTimeout(ctx, livePingInterval)
				err := conn.Ping(pingCtx)
				pingCancel()
				if err != nil {
					close(pingDone)
					return
				}
			}
		}
	}()

	// Terminal watch: the original design promised a final "closed" frame
	// that nothing sent (A0.4). Once the execution is terminal and no event
	// arrived during a whole check interval, the stream ends with one.
	// The watcher never outlives this handler: it is cancelled and awaited
	// on every return path (GitHub #364).
	var lastEvent atomic.Int64
	ended := make(chan liveEnd, 1)
	watchCtx, stopWatch := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	s.liveWatchers.Add(1)
	go func() {
		defer close(watchDone)
		defer s.liveWatchers.Add(-1)
		s.watchLiveStream(watchCtx, exec.ID, caller, &lastEvent, ended)
	}()
	defer func() {
		stopWatch()
		<-watchDone
	}()

	w := liveWriter{conn: conn, exec: exec, caller: caller, scrub: s.liveLogs().scrub,
		covers: func(p string) bool { return keyCoversProject(caller.key, p) }}
	for {
		select {
		case <-ctx.Done():
			w.endByContext(ctx)
			return
		case <-pingDone:
			return
		case why := <-ended:
			if why == liveEndRevoked {
				recordAgentStreamClosed("revoked")
				_ = conn.Close(websocket.StatusPolicyViolation, "NOT_FOUND")
				return
			}
			w.sendClosed(ctx)
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			lastEvent.Store(time.Now().UnixNano())
			if !w.write(ctx, evt) {
				return
			}
		}
	}
}

// liveWriter writes one stream's frames for one caller.
type liveWriter struct {
	conn   *websocket.Conn
	exec   *persistence.Execution
	caller liveCaller
	scrub  report.LineScrubber
	covers func(string) bool
	sent   int
}

// write projects and writes evt; false ends the stream.
func (w *liveWriter) write(ctx context.Context, evt livepubsub.LiveEvent) bool {
	out, keep := projectLiveEvent(evt, w.caller.view, w.scrub, w.covers)
	if !keep {
		return true
	}
	body, err := json.Marshal(out)
	if err != nil {
		return true
	}
	if w.caller.key != nil && w.sent+len(body) > liveAgentMaxBytes {
		recordAgentStreamClosed("budget_bytes")
		_ = w.conn.Close(websocket.StatusNormalClosure, "budget")
		return false
	}
	writeCtx, writeCancel := context.WithTimeout(ctx, liveWriteTimeout)
	err = w.conn.Write(writeCtx, websocket.MessageText, body)
	writeCancel()
	if err != nil {
		return false
	}
	w.sent += len(body)
	return true
}

// sendClosed writes the final {"kind":"closed"} frame.
func (w *liveWriter) sendClosed(ctx context.Context) {
	if body, err := json.Marshal(livepubsub.LiveEvent{ExecutionID: w.exec.ID, Seq: -1,
		Timestamp: time.Now().UTC(), Kind: livepubsub.KindClosed}); err == nil {
		writeCtx, writeCancel := context.WithTimeout(ctx, liveWriteTimeout)
		_ = w.conn.Write(writeCtx, websocket.MessageText, body)
		writeCancel()
	}
	if w.caller.key != nil {
		recordAgentStreamClosed("terminal")
	}
}

// endByContext records why an agent stream's context ended: its lifetime
// (closed with reason budget) or the client.
func (w *liveWriter) endByContext(ctx context.Context) {
	if w.caller.key == nil {
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		recordAgentStreamClosed("budget_time")
		_ = w.conn.Close(websocket.StatusNormalClosure, "budget")
		return
	}
	recordAgentStreamClosed("client")
}

// liveEnd is why the stream watcher ends a stream.
type liveEnd int

const (
	// liveEndTerminal: the execution is terminal and quiet; send closed.
	liveEndTerminal liveEnd = iota
	// liveEndRevoked: an agent key no longer may read this execution.
	liveEndRevoked
)

// watchLiveStream re-reads the execution every s.liveTerminalCheckEvery(). For an
// agent caller it re-authorises on every tick (review-20261008-f680 F1): the
// key must still be active (not revoked or expired) and still cover the
// execution's project, else the stream ends as NOT_FOUND. Any stream ends
// with the closed frame once the execution is terminal and no event came in
// the last interval. Load: one indexed execution read per stream per tick,
// plus one key lookup for an agent stream; at the global cap of 64 agent
// streams and 15 s ticks that is under 9 reads a second. An operator stream
// reads only while quiet, as before.
func (s *Server) watchLiveStream(ctx context.Context, executionID string, caller liveCaller, lastEvent *atomic.Int64, ended chan<- liveEnd) {
	every := s.liveTerminalCheckEvery()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			quiet := now.Sub(time.Unix(0, lastEvent.Load())) >= every
			if caller.key == nil && !quiet {
				// An operator stream needs no re-authorisation; while events
				// flow it reads nothing (review-20261008-2892 finding 1).
				continue
			}
			e, err := s.executionRepo.Get(ctx, executionID)
			if caller.key != nil && !s.liveCallerStillAuthorised(ctx, caller, e, err) {
				ended <- liveEndRevoked
				return
			}
			if quiet && err == nil && e != nil && isTerminalExecution(e.Status) {
				ended <- liveEndTerminal
				return
			}
		}
	}
}

// liveTaskFor is the task an execution belongs to, for view selection; when
// the task cannot be read, the execution's own project and workflow stand in.
// A transient read failure therefore keeps the previous view for at most one
// tick, the same transient policy as the execution and key re-reads
// (review-20261008-202b finding 4); a broker project still yields the broker
// view through the stand-in, which carries the execution's project.
func (s *Server) liveTaskFor(ctx context.Context, exec *persistence.Execution) *persistence.Task {
	task := &persistence.Task{ID: exec.TaskID, ProjectID: exec.ProjectID}
	if exec.WorkflowID != "" {
		wf := exec.WorkflowID
		task.WorkflowID = &wf
	}
	if s.taskRepo != nil {
		if t, err := s.taskRepo.Get(ctx, exec.TaskID); err == nil && t != nil {
			return t
		}
	}
	return task
}

// liveCallerStillAuthorised re-checks an agent stream: the execution still
// exists, the bearer still resolves to an active key, and that key still
// covers the execution's project. A transient repository error keeps the
// stream (the next tick checks again); a missing execution or key ends it.
func (s *Server) liveCallerStillAuthorised(ctx context.Context, caller liveCaller, e *persistence.Execution, getErr error) bool {
	if getErr != nil {
		return !errors.Is(getErr, persistence.ErrNotFound)
	}
	if e == nil || s.apiKeyRepo == nil {
		return false
	}
	row, err := s.apiKeyRepo.LookupActiveByHash(ctx, apikey.Hash(caller.rawKey))
	if err != nil {
		return !errors.Is(err, persistence.ErrAPIKeyNotFound)
	}
	if row == nil || row.ID != caller.key.ID || !keyCoversProject(row, e.ProjectID) {
		return false
	}
	// The view is re-derived too (review-20261008-a4ef F2): a task whose
	// project or workflow became broker mid-stream must not keep the
	// companion view. A change ends the stream; the client reconnects and
	// gets the view that now applies.
	return s.liveViewFor(row, s.liveTaskFor(ctx, e)) == caller.view
}

func liveSubscribeFromSeq(lastSeq int64) int64 {
	if lastSeq <= 0 {
		return 0
	}
	return lastSeq + 1
}

// liveHelloWait is how long the handler waits for the client's hello before
// falling back to ?last_seq=, then 0. A variable only so tests can shorten it.
var liveHelloWait = 2 * time.Second

// startLiveReader owns the connection's read side for the whole stream (live
// design, Change 2026-10-12, L2). One goroutine reads the first frame under
// the stream context, with no deadline of its own: in coder/websocket an
// expired read context closes the connection, which is how a client that sent
// no hello used to be dropped at 2 s (audit 2026-10-12, finding B). The
// handler waits for that frame up to liveHelloWait; on expiry it uses the
// fallback cursor and the stream proceeds. After the first frame the same
// goroutine hands over to conn.CloseRead, which keeps reading control frames
// and refuses any later data frame (issue #35). A hello that arrives after the
// window is read and ignored: the cursor is already fixed. A read error before
// the hello (the client went away) cancels the returned context.
//
// Exactly one reader is active at any time: conn.Read, then CloseRead's.
// stop cancels the context and so ends the goroutine.
func startLiveReader(ctx context.Context, conn *websocket.Conn, fallbackLastSeq int64) (liveClientHello, context.Context, context.CancelFunc) {
	readCtx, cancel := context.WithCancel(ctx)
	first := make(chan []byte, 1)
	go func() {
		defer cancel()
		_, body, err := conn.Read(readCtx)
		if err != nil {
			return
		}
		first <- body
		<-conn.CloseRead(readCtx).Done()
	}()
	timer := time.NewTimer(liveHelloWait)
	defer timer.Stop()
	select {
	case body := <-first:
		return parseLiveHello(body, fallbackLastSeq), readCtx, cancel
	case <-timer.C:
	case <-readCtx.Done():
	}
	return liveClientHello{LastSeq: fallbackLastSeq}, readCtx, cancel
}

// parseLiveHello decodes the client's first frame (a JSON object with
// last_seq). A body that is not that object falls back to the query cursor;
// a negative cursor means a full replay.
func parseLiveHello(body []byte, fallbackLastSeq int64) liveClientHello {
	var hello liveClientHello
	if err := json.Unmarshal(body, &hello); err != nil {
		return liveClientHello{LastSeq: fallbackLastSeq}
	}
	if hello.LastSeq < 0 {
		hello.LastSeq = 0
	}
	return hello
}

// parseLiveLastSeqFromQuery is a small helper so the JS client
// can opt in to ?last_seq=N when the hello-frame round-trip is
// awkward (e.g. on reconnect, before the new socket has had a
// chance to write). Keeps the API permissive; the hello frame
// still wins when both are set.
func parseLiveLastSeqFromQuery(r *http.Request) int64 {
	raw := r.URL.Query().Get("last_seq")
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
