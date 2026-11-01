// Package sessionstore provides the shared read-through / write-
// through helper every channel SessionStore uses to persist
// conversation state across daemon restarts + replicas.
//
// The channel stores (webchat / email / slack / github) already
// hold an in-memory map for fast Load. This package wraps the
// persistence.ChannelSessionRepository so each store gets:
//
//   - Load: check in-memory cache first, fall back to DB, populate
//     cache from DB result, return history.
//   - Save: write through to DB, then update in-memory cache.
//
// A nil repo makes both calls no-op on the DB side — the in-memory
// map remains the only state. That preserves the pre-feature
// behaviour for tests and for deployments that opt out.
//
// The fence (GitHub #344 slice 5d-2): the Persister remembers the row
// version it last read or wrote for each session. A store asks Fresh before
// serving its in-memory copy, and Save writes only over that version, so a
// session an erasure deleted or masked (from vornikctl, on any node) is
// neither served from memory nor written back.
//
// see LLD § https://docs.vornik.io §15.18
package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// Persister is the narrow surface a channel SessionStore depends
// on. Wraps persistence.ChannelSessionRepository with the
// channel-kind already bound + JSON marshalling done at this
// boundary so the channel code stays simple.
type Persister struct {
	repo   persistence.ChannelSessionRepository
	kind   string
	logger zerolog.Logger

	// OnFenceError is called when a freshness read fails (the
	// vornik_channel_session_fence_errors_total counter). Nil is allowed.
	OnFenceError func()

	mu sync.Mutex
	// seen is the row state this process last read or wrote per session.
	// A missing key is unknown, which is never fresh.
	seen map[string]rowState
}

// rowState is a session row as last seen: absent, or present at a version.
type rowState struct {
	found   bool
	version int64
}

// ErrSessionMoved reports a save refused because the stored row changed
// since this process last saw it: another node's turn, or an erasure. The
// caller drops its in-memory copy; the next turn starts from the row.
var ErrSessionMoved = errors.New("session_store: the stored session changed since it was loaded; not saved")

// New constructs a Persister bound to a channel kind. The kind
// goes into the (kind, session_id) composite PK so different
// channels can't collide on a shared session_id (e.g. an integer
// chat_id reused as a webchat cookie hash). repo may be nil —
// every method then becomes a no-op + ErrNotFound, falling back
// to the caller's in-memory cache.
func New(repo persistence.ChannelSessionRepository, kind string, logger zerolog.Logger) *Persister {
	return &Persister{repo: repo, kind: kind, logger: logger, seen: map[string]rowState{}}
}

// Load returns the persisted history + active project for
// sessionID. (nil, "", false, nil) when no DB row exists or when
// the repo is unwired — the channel falls back to its in-memory
// cache. Errors other than ErrNotFound propagate so the channel
// can decide whether to log + serve from cache or fail-soft.
func (p *Persister) Load(ctx context.Context, sessionID string) (history []chat.Message, activeProject string, found bool, err error) {
	if p == nil || p.repo == nil {
		return nil, "", false, nil
	}
	row, err := p.repo.Load(ctx, p.kind, sessionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			p.see(sessionID, rowState{})
			return nil, "", false, nil
		}
		p.Forget(sessionID)
		return nil, "", false, err
	}
	p.see(sessionID, rowState{found: true, version: row.Version})
	if len(row.History) > 0 {
		if jsonErr := json.Unmarshal(row.History, &history); jsonErr != nil {
			// Corrupt-row tolerance: bad JSON shouldn't break the
			// channel. Log + serve empty history; next turn will
			// rewrite the row with valid bytes.
			p.logger.Warn().Err(jsonErr).
				Str("kind", p.kind).
				Str("session_id", sessionID).
				Msg("session_store: corrupt history JSON; serving empty")
			history = nil
		}
	}
	return history, row.ActiveProject, true, nil
}

// Save write-through-persists the post-turn history + active project, but
// only over the row version this process last saw (absent, or a version).
// No-op when the repo is unwired. The channel's in-memory cache is the
// caller's responsibility — Save only writes the durable row.
//
// ErrSessionMoved means the row changed underneath (another node, or an
// erasure): nothing was written and the caller must drop its in-memory copy
// of the session. Any other error is a database failure; the session's
// state becomes unknown, so the next Fresh is false and the store reloads
// (fail closed, §15.18).
func (p *Persister) Save(ctx context.Context, sessionID, activeProject string, history []chat.Message) error {
	if p == nil || p.repo == nil {
		return nil
	}
	raw, err := json.Marshal(history)
	if err != nil {
		// Should never happen — chat.Message is plain JSON-tagged.
		p.logger.Error().Err(err).
			Str("kind", p.kind).
			Str("session_id", sessionID).
			Msg("session_store: marshal history; skipping persist")
		return nil
	}
	var expect *int64
	p.mu.Lock()
	st, known := p.seen[sessionID]
	p.mu.Unlock()
	if known && st.found {
		v := st.version
		expect = &v
	}
	v, ok, err := p.repo.SaveIfVersion(ctx, p.kind, sessionID, activeProject, raw, expect)
	switch {
	case err != nil:
		p.Forget(sessionID)
		p.logger.Warn().Err(err).
			Str("kind", p.kind).
			Str("session_id", sessionID).
			Msg("session_store: persist failed; the session reloads on its next turn")
		return err
	case !ok:
		p.Forget(sessionID)
		p.logger.Info().
			Str("kind", p.kind).
			Str("session_id", sessionID).
			Msg("session_store: the stored session moved (another node or an erasure); dropping the cached copy")
		return ErrSessionMoved
	}
	if p.repo.Fenced() {
		p.see(sessionID, rowState{found: true, version: v})
	}
	return nil
}

// Fresh reports whether this process's in-memory copy of the session may be
// served: the stored row is exactly as this process last read or wrote it.
// A changed or deleted row, a session never seen, or a failed read is not
// fresh (fail closed). Always true where nothing durable is stored (no repo,
// or an unfenced one).
func (p *Persister) Fresh(ctx context.Context, sessionID string) bool {
	if p == nil || p.repo == nil || !p.repo.Fenced() {
		return true
	}
	found, v, err := p.repo.Version(ctx, p.kind, sessionID)
	if err != nil {
		if p.OnFenceError != nil {
			p.OnFenceError()
		}
		p.logger.Warn().Err(err).
			Str("kind", p.kind).
			Str("session_id", sessionID).
			Msg("session_store: fence read failed; not serving the cached session")
		p.Forget(sessionID)
		return false
	}
	p.mu.Lock()
	st, known := p.seen[sessionID]
	p.mu.Unlock()
	return known && st == rowState{found: found, version: v}
}

// Fenced reports whether a durable, versioned store backs this persister
// (Postgres). False for nil, unwired, or the SQLite stub.
func (p *Persister) Fenced() bool {
	return p != nil && p.repo != nil && p.repo.Fenced()
}

// Forget drops what this process saw of the session, so its next Fresh is
// false. Called when the store drops the session from memory.
func (p *Persister) Forget(sessionID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.seen, sessionID)
	p.mu.Unlock()
}

func (p *Persister) see(sessionID string, st rowState) {
	if !p.repo.Fenced() {
		return
	}
	p.mu.Lock()
	p.seen[sessionID] = st
	p.mu.Unlock()
}

// SiblingSession is one persisted session under a shared SessionID prefix,
// with its history already unmarshalled.
type SiblingSession struct {
	SessionID string
	History   []chat.Message
	UpdatedAt time.Time
}

// ListByPrefix returns the persisted sessions whose SessionID starts with
// prefix, most-recently-updated first, capped at limit. Nil when the repo is
// unwired (SQLite / opted-out deployments) — callers fall back to whatever
// in-memory view they have rather than treating it as an error.
//
// Corrupt-history rows are skipped rather than failing the batch, matching
// Load's corrupt-row tolerance: one bad row must not deny the caller the other
// siblings.
func (p *Persister) ListByPrefix(ctx context.Context, prefix string, limit int) ([]SiblingSession, error) {
	if p == nil || p.repo == nil {
		return nil, nil
	}
	rows, err := p.repo.ListByPrefix(ctx, p.kind, prefix, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SiblingSession, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		var history []chat.Message
		if len(row.History) > 0 {
			if jsonErr := json.Unmarshal(row.History, &history); jsonErr != nil {
				p.logger.Warn().Err(jsonErr).
					Str("kind", p.kind).
					Str("session_id", row.SessionID).
					Msg("session_store: corrupt history JSON in prefix listing; skipping row")
				continue
			}
		}
		out = append(out, SiblingSession{
			SessionID: row.SessionID,
			History:   history,
			UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

// Delete removes the persisted row. Used by "clear chat" affordances.
func (p *Persister) Delete(ctx context.Context, sessionID string) error {
	if p == nil || p.repo == nil {
		return nil
	}
	if err := p.repo.Delete(ctx, p.kind, sessionID); err != nil {
		p.Forget(sessionID)
		return err
	}
	// Our own clear: the row is absent and this process knows it.
	p.see(sessionID, rowState{})
	return nil
}
