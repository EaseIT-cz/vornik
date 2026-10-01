// Cross-replica live-events publisher (horizontal-scaling
// follow-on). Wraps the in-process publisher with:
//
//   - Persistence via persistence.ExecutionLiveEventRepository:
//     every Publish appends a row, so a non-emitting replica or a
//     late-joining subscriber can replay.
//   - Cross-replica fanout via Postgres NOTIFY: the emitting
//     replica fires `NOTIFY vornik_live, "<execID>|<seq>|<nodeID>"`,
//     and every other replica's LISTEN goroutine ingests the
//     matching row into its local in-process ring so its
//     subscribers see the same stream.
//
// Single-process / SQLite deployments don't need any of this —
// the bare inProcessPublisher already covers them. The container
// constructs a dbBackedPublisher only when the repo is wired (the
// Postgres branch) AND a notifier/listener pair is supplied.
//
// Failure mode: an Append or NOTIFY error doesn't break the local
// stream. The wrapper logs + falls back to in-process-only
// delivery. The user watching this replica's stream still sees
// every event their own replica produced.

package livepubsub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Notifier sends a Postgres NOTIFY to the channel. Implementations:
// PostgresNotifier (production) wraps *sql.DB.ExecContext; tests
// use stub implementations that record calls.
type Notifier interface {
	Notify(ctx context.Context, channel, payload string) error
}

// Listener consumes Postgres LISTEN notifications and feeds them
// on the returned channel. Cancelling the supplied context is the
// caller's shutdown signal; the listener should drain + close.
type Listener interface {
	// Start subscribes to the channel and returns a stream of
	// notification payloads. Returns an error if the initial
	// LISTEN handshake failed.
	Start(ctx context.Context, channel string) (<-chan Notification, error)
}

// Notification mirrors lib/pq's notification struct without the
// pq dependency leaking out of the postgres-specific driver.
type Notification struct {
	Channel string
	Payload string
}

// NotifyChannel is the Postgres LISTEN/NOTIFY channel every
// replica subscribes to. One channel for all executions; the
// payload format below carries the execution_id so the listener
// can filter.
const NotifyChannel = "vornik_live"

// dbBackedPublisher implements Publisher by composing an
// in-process publisher (for local fanout) with a DB repository
// + Notifier (for cross-replica fanout).
type dbBackedPublisher struct {
	inner    *inProcessPublisher
	repo     persistence.ExecutionLiveEventRepository
	notifier Notifier
	logger   zerolog.Logger
	nodeID   string

	// detachedBound caps each I/O phase of Publish (the append, then the
	// NOTIFY), which run detached from the caller's cancellation so a killed
	// step's last events survive it. A field so tests can shorten it.
	detachedBound time.Duration

	// listenerCtx + cancel control the LISTEN goroutine. nil when
	// the wrapper is constructed without a listener (NOTIFY-only
	// deployments — they still cross-replicate but only one-way).
	listenerCancel context.CancelFunc
	listenerDone   chan struct{}

	// stopMu serialises Close calls.
	stopMu sync.Mutex
	closed bool
}

// NewDBBackedConfig bundles the dependencies a cross-replica
// publisher needs. nodeID identifies this daemon so the
// LISTEN goroutine can drop self-emitted notifications.
type NewDBBackedConfig struct {
	Inner    *inProcessPublisher
	Repo     persistence.ExecutionLiveEventRepository
	Notifier Notifier
	Listener Listener
	NodeID   string
	Logger   zerolog.Logger
}

// NewDBBacked constructs the wrapper + starts the LISTEN
// goroutine (if a listener was supplied). The returned shutdown
// closure cancels the goroutine and waits for it to drain;
// callers defer it in the container's shutdown sequence.
//
// inner may be nil — the wrapper allocates a default in-process
// publisher in that case.
func NewDBBacked(ctx context.Context, cfg NewDBBackedConfig) (Publisher, func(), error) {
	if cfg.Repo == nil {
		return nil, nil, errors.New("livepubsub: NewDBBacked requires Repo")
	}
	inner := cfg.Inner
	if inner == nil {
		inner = &inProcessPublisher{streams: map[string]*stream{}, ringSize: envInt("VORNIK_LIVE_RING_SIZE", 200)}
	}
	p := &dbBackedPublisher{
		inner:         inner,
		repo:          cfg.Repo,
		notifier:      cfg.Notifier,
		logger:        cfg.Logger,
		nodeID:        cfg.NodeID,
		detachedBound: defaultDetachedBound,
	}
	if cfg.Listener != nil {
		lctx, cancel := context.WithCancel(ctx)
		p.listenerCancel = cancel
		p.listenerDone = make(chan struct{})
		notifications, err := cfg.Listener.Start(lctx, NotifyChannel)
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("livepubsub: start listener: %w", err)
		}
		go p.runListenLoop(lctx, notifications)
	}
	return p, p.close, nil
}

// Publish writes the event to the DB, NOTIFY-broadcasts the
// (execID, seq, nodeID) tuple to every other replica, and fans
// out locally. Returns the DB-allocated seq.
//
// On DB failure, falls back to local-only Publish via the inner
// publisher (so a transient blip doesn't blank the user's
// stream). Operators see a single warn log; metrics on the DB
// side surface the underlying error.
func (p *dbBackedPublisher) Publish(ctx context.Context, executionID, kind string, payload any) int64 {
	if executionID == "" || kind == "" {
		return 0
	}
	raw, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		// Should never happen for the documented payload structs.
		// Log + fall back to a degraded local publish without DB.
		p.logger.Warn().Err(marshalErr).
			Str("execution_id", executionID).
			Str("kind", kind).
			Msg("livepubsub: marshal failed; falling back to in-process publish")
		return p.inner.Publish(ctx, executionID, kind, payload)
	}
	// The append and the NOTIFY run on the caller's values but NOT its
	// cancellation, each under its own fresh bound: a step killed at its
	// budget publishes its last events on a cancelled context, and those are
	// the record of what just happened (live-task-observation design,
	// amendment 2026-09-25).
	detached := context.WithoutCancel(ctx)
	appendCtx, cancelAppend := context.WithTimeout(detached, p.detachedBound)
	seq, err := p.repo.Append(appendCtx, executionID, kind, raw)
	timedOut := appendCtx.Err() != nil
	cancelAppend()
	if err != nil {
		// DB blip — keep the local stream alive so the user doesn't
		// see a gap. Cross-replica fanout is lost for this event;
		// the next successful Append re-syncs. The in-process publish
		// never reads its context, so the spent bound cannot stop it
		// (pinned by TestDBBackedPublisher_AppendBoundExpiryFallsBackLocally).
		cause := p.causeOf(timedOut, "append")
		p.logger.Warn().Err(err).
			Str("execution_id", executionID).
			Str("kind", kind).
			Str("cause", cause).
			Msg("livepubsub: DB append failed; falling back to in-process publish")
		return p.inner.Publish(appendCtx, executionID, kind, payload)
	}

	// Local delivery uses the DB-authoritative seq + timestamp so
	// every replica's subscriber sees the same wire-format.
	evt := LiveEvent{
		ExecutionID: executionID,
		Seq:         seq,
		Timestamp:   time.Now().UTC(),
		Kind:        kind,
		Payload:     payload,
	}
	p.inner.IngestRemote(evt)

	// Cross-replica NOTIFY. Failures here are non-fatal — the
	// local stream is already served, and downstream replicas
	// only miss this one event (they'll catch up on the next
	// notification's ListSince fallback).
	if p.notifier != nil {
		notif := fmt.Sprintf("%s|%d|%s", executionID, seq, p.nodeID)
		notifyCtx, cancelNotify := context.WithTimeout(detached, p.detachedBound)
		err := p.notifier.Notify(notifyCtx, NotifyChannel, notif)
		timedOut := notifyCtx.Err() != nil
		cancelNotify()
		if err != nil {
			p.logger.Warn().Err(err).
				Str("execution_id", executionID).
				Int64("seq", seq).
				Str("cause", p.causeOf(timedOut, "notify")).
				Msg("livepubsub: NOTIFY failed; cross-replica fanout will miss this event")
		}
	}
	return seq
}

// defaultDetachedBound: the append is an INSERT into the daemon's own
// database, normally milliseconds; what lost the record was cancellation, not
// latency. 5 s is three orders over a healthy append and still short enough
// that a wedged database cannot pin a finished step's goroutine.
const defaultDetachedBound = 5 * time.Second

// causeOf labels a failed publish phase for the warning, counting it when the
// phase ran out its own detached bound.
func (p *dbBackedPublisher) causeOf(timedOut bool, phase string) string {
	if !timedOut {
		return "db_error"
	}
	if m := p.inner.metrics; m != nil && m.DetachedTimeoutTotal != nil {
		m.DetachedTimeoutTotal.WithLabelValues(phase).Inc()
	}
	return "detached_timeout"
}

// Subscribe delegates to the in-process publisher's subscribe path with a
// PRIVATE DB backfill: when the requested fromSeq is older than the ring (or
// the ring is empty — a fresh subscriber on this replica, or any page load
// after a daemon restart), the missing history is read from the DB and
// served to THIS subscriber only, ahead of the live stream.
//
// Pre-2026-09-28 the history was injected through IngestRemote (the live
// deliver path): it was appended to the end of the shared ring, evicting the
// newest events, and fanned out to every other subscriber and the fleet tap,
// so other open pages flipped finished steps back to "running" (T-0d3c).
// fromSeq=0 goes through the same probe — pre-2026-07-12 it was excluded, so
// after a restart the live page rendered no history at all.
func (p *dbBackedPublisher) Subscribe(executionID string, fromSeq int64) (<-chan LiveEvent, func(), error) {
	return p.inner.subscribe(executionID, fromSeq, func(ringOldest int64) []LiveEvent {
		if ringOldest >= 0 && fromSeq >= ringOldest {
			return nil // the ring snapshot covers the cursor
		}
		return p.fetchHistory(executionID, fromSeq, ringOldest)
	})
}

// SubscribeAll delegates to the inner in-process publisher's fleet tap.
// Cross-replica events arrive via the LISTEN loop → IngestRemote → inner
// deliver, which fans to the inner's fleet subscribers — so this single tap
// sees events from every replica, not just the local one. History replayed
// for a per-execution subscriber never reaches it.
func (p *dbBackedPublisher) SubscribeAll() (<-chan LiveEvent, func(), error) {
	return p.inner.SubscribeAll()
}

// replayLimit caps one subscriber's DB history — a subscriber asking for
// fromSeq=0 on an execution with 100k events would otherwise blow up memory
// and its channel buffer. The window served is the NEWEST replayLimit rows
// ending where the ring begins, so the subscriber always receives a
// contiguous suffix of the stream; the ReplayGapMarker announces a clipped
// start. (Pre-2026-09-28 it was the OLDEST 2000 rows from fromSeq, which on
// a long execution ended far below the ring — an unreported hole.)
const replayLimit = 2000

// fetchHistory reads the events [lo, upper) for one subscriber. upper is
// ringOldest, or LatestSeq+1 when the ring was empty; lo is
// max(fromSeq, upper-replayLimit). With an empty ring the read is not capped
// at upper: rows appended after LatestSeq are returned too and deduplicated
// against the subscriber's pending events. Errors are logged and yield no
// history — the subscriber gets the ring-only replay, as before.
func (p *dbBackedPublisher) fetchHistory(executionID string, fromSeq, ringOldest int64) []LiveEvent {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	upper := ringOldest
	if upper < 0 {
		latest, err := p.repo.LatestSeq(ctx, executionID)
		if err != nil {
			p.logger.Warn().Err(err).
				Str("execution_id", executionID).
				Int64("from_seq", fromSeq).
				Msg("livepubsub: DB replay failed; subscriber will get ring-only replay")
			return nil
		}
		upper = latest + 1
	}
	lo := fromSeq
	if lo < 0 {
		lo = 0
	}
	if upper-lo > replayLimit {
		lo = upper - replayLimit
	}
	if upper <= lo {
		return nil
	}
	rows, err := p.repo.ListSince(ctx, executionID, lo, replayLimit)
	if err != nil {
		p.logger.Warn().Err(err).
			Str("execution_id", executionID).
			Int64("from_seq", fromSeq).
			Msg("livepubsub: DB replay failed; subscriber will get ring-only replay")
		return nil
	}
	out := make([]LiveEvent, 0, len(rows))
	for _, row := range rows {
		if ringOldest >= 0 && row.Seq >= ringOldest {
			break // the ring snapshot serves these
		}
		var payload any
		if len(row.Payload) > 0 {
			_ = json.Unmarshal(row.Payload, &payload)
		}
		out = append(out, LiveEvent{
			ExecutionID: row.ExecutionID,
			Seq:         row.Seq,
			Timestamp:   row.CreatedAt,
			Kind:        row.Kind,
			Payload:     payload,
		})
	}
	return out
}

// runListenLoop consumes Postgres notifications from the listener
// channel and ingests each remote event into the local in-process
// publisher. Self-emitted notifications are skipped (we already
// fanned out locally in Publish).
//
// On listener-channel close (driver-side reconnect failure or
// context cancellation), the loop returns and signals done.
// Operators see the underlying error via the listener's own
// logging path; production wires the postgres listener with
// auto-reconnect, so a transient blip recovers without daemon
// intervention.
func (p *dbBackedPublisher) runListenLoop(ctx context.Context, notifications <-chan Notification) {
	defer close(p.listenerDone)
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-notifications:
			if !ok {
				p.logger.Warn().Msg("livepubsub: listener channel closed; cross-replica fanout stopped")
				return
			}
			p.handleNotification(ctx, n.Payload)
		}
	}
}

// handleNotification parses one notification payload of the form
// "<execID>|<seq>|<nodeID>" and ingests the matching DB row into
// the local in-process publisher. Self-emitted notifications
// (matching nodeID) are skipped to avoid double-fanout.
func (p *dbBackedPublisher) handleNotification(ctx context.Context, payload string) {
	execID, seq, emitterNodeID, ok := parseNotifyPayload(payload)
	if !ok {
		p.logger.Warn().Str("payload", payload).Msg("livepubsub: malformed NOTIFY payload; skipping")
		return
	}
	if p.nodeID != "" && emitterNodeID == p.nodeID {
		// Self-emit — we already fanned out locally during Publish.
		return
	}

	// Pull the exact row from DB. Bounded latency: <5ms typical
	// under normal load. Cross-replica latency is dominated by
	// the LISTEN/NOTIFY hop + this single-row SELECT.
	rows, err := p.repo.ListSince(ctx, execID, seq, 1)
	if err != nil {
		p.logger.Warn().Err(err).
			Str("execution_id", execID).
			Int64("seq", seq).
			Msg("livepubsub: failed to fetch remote event; skipping")
		return
	}
	for _, row := range rows {
		if row.Seq != seq {
			continue
		}
		var pl any
		if len(row.Payload) > 0 {
			_ = json.Unmarshal(row.Payload, &pl)
		}
		p.inner.IngestRemote(LiveEvent{
			ExecutionID: row.ExecutionID,
			Seq:         row.Seq,
			Timestamp:   row.CreatedAt,
			Kind:        row.Kind,
			Payload:     pl,
		})
		return
	}
}

// parseNotifyPayload splits "<execID>|<seq>|<nodeID>" into its
// three components. seq must parse as an int64. Returns false on
// any parse failure — the caller logs + drops.
func parseNotifyPayload(s string) (execID string, seq int64, nodeID string, ok bool) {
	parts := strings.SplitN(s, "|", 3)
	if len(parts) != 3 {
		return "", 0, "", false
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, "", false
	}
	return parts[0], n, parts[2], true
}

// close cancels the listener goroutine and waits for it to drain.
// Idempotent; subsequent calls return immediately.
func (p *dbBackedPublisher) close() {
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.listenerCancel != nil {
		p.listenerCancel()
	}
	if p.listenerDone != nil {
		<-p.listenerDone
	}
}
