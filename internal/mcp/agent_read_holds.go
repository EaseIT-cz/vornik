package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Sticky suspension and the periodic re-list of agent read servers
// (agent-administered design §6.5, amendment 2026-10-21, GitHub #481;
// operator decision A7). §6.5 refuses an agent read call whose tool the
// client's own listing calls mutating; that verdict is derived per client,
// so a server that dropped the mark at its next listing got the tool back
// as a read with nobody approving it. Here a suspension is also held: in
// memory for the gate, and stored on the approval row, from which only a
// re-add the person approves on the device clears it. And a connected
// agent read server is listed again every interval, so a descriptor change
// between reloads is judged.

// ErrReadSuspensionsUnread refuses an agent read call before the stored
// suspensions have been read: until then nothing says the tool is not held.
// Nothing was sent.
var ErrReadSuspensionsUnread = errors.New("the record of suspended agent read tools has not been read yet, so agent read tools are refused until it is")

// ErrReadListingStale refuses an agent read call whose server's listing the
// re-list has not been able to renew for twice the interval: a server may
// refuse new sessions while serving the old one. Nothing was sent.
var ErrReadListingStale = errors.New("the server's tool listing could not be renewed, so its tools no longer run as reads until it can")

// ErrSuspensionMoot marks a suspension write that has nothing to suspend:
// no live approval row (the approval gate refuses the call first), or a
// row approved after the listing that judged it. It is not retried.
var ErrSuspensionMoot = errors.New("mcp: nothing to suspend")

// ErrSuspensionNoApproval marks a suspension write that found no live
// approval row yet: the listing that judged it came from an apply whose
// grant is written after its reload (an add or a re-add the device just
// approved). It is retried at every tick while the manager still has a
// client for the server, so a mark seen on the first session after an
// approval is stored once the row is live (code review, adversarial F1).
var ErrSuspensionNoApproval = errors.New("mcp: no live approval to suspend yet")

// ReadSuspensionStore persists held read tools. The daemon adapts the
// agent grant repository; nil keeps the holds in memory only.
type ReadSuspensionStore interface {
	// LoadReadSuspensions returns every live held set: project, server, tools.
	LoadReadSuspensions(ctx context.Context) (map[string]map[string][]string, error)
	// SuspendReadTools adds tools to the held set of one live approval,
	// judged from a listing taken at listedAt. ErrSuspensionMoot (wrapped)
	// when the approval is newer than the listing; ErrSuspensionNoApproval
	// (wrapped) when there is no live approval row.
	SuspendReadTools(ctx context.Context, projectID, server string, tools []string, listedAt time.Time) error
}

// UnavailableReadSuspensions is the store of a daemon that should have one
// and does not: every load fails, so agent read calls stay refused
// (ErrReadSuspensionsUnread) rather than running with no stickiness (code
// review 3450 F2).
type UnavailableReadSuspensions struct{ Why string }

// LoadReadSuspensions implements ReadSuspensionStore: it always fails.
func (u UnavailableReadSuspensions) LoadReadSuspensions(context.Context) (map[string]map[string][]string, error) {
	return nil, errors.New("suspension store unavailable: " + u.Why)
}

// SuspendReadTools implements ReadSuspensionStore: it always fails.
func (u UnavailableReadSuspensions) SuspendReadTools(context.Context, string, string, []string, time.Time) error {
	return errors.New("suspension store unavailable: " + u.Why)
}

// suspendWriteTimeout bounds one suspension write on the dial path.
const suspendWriteTimeout = 5 * time.Second

// relistTick is the re-list loop's cadence: a failed re-list is retried at
// the next tick, and an unread store is read again.
const relistTick = time.Minute

// pendingWrite is a suspension memory holds but the store has not taken.
type pendingWrite struct {
	tools    map[string]bool
	listedAt time.Time
}

// readHolds is the manager's held set. Its own mutex: the gate runs under
// the manager's read lock (callOnce), and a load must not wait on it.
type readHolds struct {
	mu      sync.RWMutex
	store   ReadSuspensionStore
	loaded  bool
	loadErr error
	held    map[string]map[string]map[string]bool // project → server → tool
	retry   map[[2]string]*pendingWrite           // {project, server}
	// interval is the re-list interval while RunAgentReadRelist runs, 0
	// otherwise; listings age out only while it is set.
	interval time.Duration
	// writeSeq counts stored suspensions: a load that raced one keeps the
	// set it had as well as the stored one (adversarial review F3).
	writeSeq uint64
}

func (h *readHolds) holdsLocked(projectID, server, tool string) bool {
	return h.held[projectID][server][tool]
}

func (h *readHolds) addLocked(projectID, server string, tools []string) {
	if h.held == nil {
		h.held = map[string]map[string]map[string]bool{}
	}
	if h.held[projectID] == nil {
		h.held[projectID] = map[string]map[string]bool{}
	}
	if h.held[projectID][server] == nil {
		h.held[projectID][server] = map[string]bool{}
	}
	for _, t := range tools {
		h.held[projectID][server][t] = true
	}
}

// queueLocked puts tools in the retry set: held by memory, not yet taken
// by the store. Caller holds h.mu.
func (h *readHolds) queueLocked(projectID, server string, tools []string, listedAt time.Time) {
	if h.retry == nil {
		h.retry = map[[2]string]*pendingWrite{}
	}
	key := [2]string{projectID, server}
	w := h.retry[key]
	if w == nil {
		w = &pendingWrite{tools: map[string]bool{}, listedAt: listedAt}
		h.retry[key] = w
	}
	for _, t := range tools {
		w.tools[t] = true
	}
	if listedAt.After(w.listedAt) {
		w.listedAt = listedAt
	}
}

// SetReadSuspensions wires the store. Until its first successful load,
// agent read calls are refused (ErrReadSuspensionsUnread).
func (m *Manager) SetReadSuspensions(store ReadSuspensionStore) {
	m.holds.mu.Lock()
	defer m.holds.mu.Unlock()
	if m.holds.store == store {
		return
	}
	m.holds.store, m.holds.loaded, m.holds.loadErr = store, false, nil
}

// LoadReadSuspensions replaces the held set with the stored one plus the
// suspensions the store has not yet taken. A failed load keeps the held set
// it had (and an unread store stays unread).
func (m *Manager) LoadReadSuspensions(ctx context.Context) error {
	m.holds.mu.RLock()
	store, seq := m.holds.store, m.holds.writeSeq
	m.holds.mu.RUnlock()
	if store == nil {
		return nil
	}
	stored, err := store.LoadReadSuspensions(ctx)
	m.holds.mu.Lock()
	defer m.holds.mu.Unlock()
	if err != nil {
		m.holds.loadErr = err
		m.logger.Error().Err(err).Msg("mcp: could not read the suspended agent read tools; held set unchanged")
		return err
	}
	if m.holds.writeSeq == seq {
		// No suspension was stored while this read ran, so the stored set
		// is complete; otherwise keep what memory holds too (fail closed:
		// at worst an over-hold until the next load).
		m.holds.held = nil
	}
	for projectID, byServer := range stored {
		for server, tools := range byServer {
			m.holds.addLocked(projectID, server, tools)
		}
	}
	for k, w := range m.holds.retry {
		m.holds.addLocked(k[0], k[1], sortedKeys(w.tools))
	}
	m.holds.loaded, m.holds.loadErr = true, nil
	return nil
}

// ReadHoldState is the doctor's view of the held set's store and the
// re-list (amendment #481): whether a store is wired, whether it has been
// read, the last load's error, and the re-list interval while the loop
// runs (0 otherwise).
type ReadHoldState struct {
	Wired, Loaded bool
	LoadErr       error
	Interval      time.Duration
}

// ReadHoldState returns the store and re-list state.
func (m *Manager) ReadHoldState() ReadHoldState {
	m.holds.mu.RLock()
	defer m.holds.mu.RUnlock()
	return ReadHoldState{Wired: m.holds.store != nil, Loaded: m.holds.store == nil || m.holds.loaded,
		LoadErr: m.holds.loadErr, Interval: m.holds.interval}
}

func (m *Manager) setRelistInterval(d time.Duration) {
	m.holds.mu.Lock()
	m.holds.interval = d
	m.holds.mu.Unlock()
}

// holdRefusal is the gate's check of the held set and the listing's age
// for one agent read call, after the client's own verdict admitted it.
func (m *Manager) holdRefusal(projectID, server, tool string, c *Client) error {
	m.holds.mu.RLock()
	unread := m.holds.store != nil && !m.holds.loaded
	held := m.holds.holdsLocked(projectID, server, tool)
	interval := m.holds.interval
	m.holds.mu.RUnlock()
	switch {
	case unread:
		return fmt.Errorf("%w (MCP server %q, tool %s)", ErrReadSuspensionsUnread, server, tool)
	case held:
		return fmt.Errorf("%w (MCP server %q described %s as a tool that changes things; it stays suspended, even though the server "+
			"no longer says so, until the person approves it again on their device: the agent removes the server and adds it again)",
			ErrReadToolChangesThings, server, tool)
	case interval > 0 && !c.listedAt.IsZero() && m.now().Sub(c.listedAt) > 2*interval:
		return fmt.Errorf("%w (MCP server %q was last listed %s ago)", ErrReadListingStale, server, m.now().Sub(c.listedAt).Round(time.Second))
	}
	return nil
}

// judgeListing runs at every client build: it logs an agent read entry
// whose listing suspends approved tools (§6.5 item 4) and holds the
// suspended ones, in memory first and then in the store (amendment #481).
// A build that loses its install still records: its server did describe
// the tool as one that changes things.
func (m *Manager) judgeListing(projectID string, c *Client) {
	m.logSuspendedReads(projectID, c)
	if c == nil || !agentReadEntry(projectID, c.Name()) {
		return
	}
	_, suspended, _ := c.suspendedReads()
	if len(suspended) == 0 {
		return
	}
	server := c.Name()
	m.holds.mu.Lock()
	var fresh []string
	for _, t := range suspended {
		if !m.holds.holdsLocked(projectID, server, t) {
			fresh = append(fresh, t)
		}
	}
	m.holds.addLocked(projectID, server, fresh)
	store := m.holds.store
	if store != nil && len(fresh) > 0 {
		// In the retry set until the store has it, so a load racing this
		// write (a reload's) keeps holding it; writeSuspension removes it.
		m.holds.queueLocked(projectID, server, fresh, c.listedAt)
	}
	m.holds.mu.Unlock()
	if len(fresh) == 0 || store == nil {
		return
	}
	m.writeSuspension(store, projectID, server, fresh, c.listedAt)
}

// writeSuspension stores one suspension, or keeps it for the next pass. It
// reports whether the write was moot (nothing to suspend).
func (m *Manager) writeSuspension(store ReadSuspensionStore, projectID, server string, tools []string, listedAt time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), suspendWriteTimeout)
	defer cancel()
	err := store.SuspendReadTools(ctx, projectID, server, tools, listedAt)
	key := [2]string{projectID, server}
	m.holds.mu.Lock()
	defer m.holds.mu.Unlock()
	switch {
	case errors.Is(err, ErrSuspensionNoApproval):
		m.holds.queueLocked(projectID, server, tools, listedAt)
		m.logger.Info().Err(err).Str("project", projectID).Str("server", server).Strs("tools", tools).
			Msg("mcp: suspension of agent read tools waits for its approval row; held in memory and retried at the next re-list tick")
		return false
	case err == nil || errors.Is(err, ErrSuspensionMoot):
		if err == nil {
			// Pinned again: memory must hold what the store now holds,
			// whatever a load did meanwhile (code review 3450 F1).
			m.holds.addLocked(projectID, server, tools)
			m.holds.writeSeq++
		}
		if w := m.holds.retry[key]; w != nil {
			for _, t := range tools {
				delete(w.tools, t)
			}
			if len(w.tools) == 0 {
				delete(m.holds.retry, key)
			}
		}
		if err != nil {
			m.logger.Info().Err(err).Str("project", projectID).Str("server", server).Strs("tools", tools).
				Msg("mcp: suspension of agent read tools not stored")
		}
		return err != nil
	default:
		m.holds.queueLocked(projectID, server, tools, listedAt)
		m.logger.Error().Err(err).Str("project", projectID).Str("server", server).Strs("tools", tools).
			Msg("mcp: could not store the suspension of agent read tools; held in memory and retried at the next re-list tick")
		return false
	}
}

// retrySuspensionWrites writes again what the store has not taken. A moot
// retry (the approval was re-approved since that listing) is dropped, and
// the store is read again so memory stops holding it. A retry for a server
// no longer configured is dropped without a load.
func (m *Manager) retrySuspensionWrites(ctx context.Context) {
	m.holds.mu.RLock()
	store := m.holds.store
	type job struct {
		project, server string
		tools           []string
		listedAt        time.Time
	}
	var jobs []job
	for k, w := range m.holds.retry {
		jobs = append(jobs, job{k[0], k[1], sortedKeys(w.tools), w.listedAt})
	}
	m.holds.mu.RUnlock()
	if store == nil {
		return
	}
	moot := false
	for _, j := range jobs {
		if !m.serverConfigured(j.project, j.server) {
			// The server is gone from the project's configuration (removed):
			// only a new approval brings it back, and a listing taken before
			// that cannot suspend it. A server that is merely disconnected,
			// pending a dial or mid-reconnect is still configured and keeps
			// its queued write (security review, fail closed). Only the
			// snapshot's tools go, and only if no newer listing re-queued
			// them; no clearing load follows, so memory keeps holding them
			// until an ordinary load.
			m.holds.mu.Lock()
			key := [2]string{j.project, j.server}
			if w := m.holds.retry[key]; w != nil && w.listedAt.Equal(j.listedAt) {
				for _, t := range j.tools {
					delete(w.tools, t)
				}
				if len(w.tools) == 0 {
					delete(m.holds.retry, key)
				}
			}
			m.holds.mu.Unlock()
			continue
		}
		moot = m.writeSuspension(store, j.project, j.server, j.tools, j.listedAt) || moot
	}
	if moot {
		_ = m.LoadReadSuspensions(ctx)
	}
}

// serverConfigured reports whether (project, server) is configured in the
// manager's catalogue: connected, pending a dial, or deliberately withheld.
func (m *Manager) serverConfigured(projectID, serverName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[projectID][serverName] != nil || m.pending[projectID][serverName] != nil ||
		m.withheld[projectID][serverName] != ""
}

// heldListed are the held tools of a client's allowlist its current
// listing would let run (listed, not mutating): what every surface reports
// as "the server no longer says so".
func (m *Manager) heldListed(projectID string, c *Client) []string {
	v := c.readVerdicts()
	m.holds.mu.RLock()
	defer m.holds.mu.RUnlock()
	var out []string
	for t := range m.holds.held[projectID][c.Name()] {
		if mutating, listed := v[t]; listed && !mutating {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// HeldReads returns one agent read entry's held tools its current listing
// would let run, sorted; nil when it has no client.
func (m *Manager) HeldReads(projectID, serverName string) []string {
	if !agentReadEntry(projectID, serverName) {
		return nil
	}
	c := m.resolveClient(projectID, serverName)
	if c == nil {
		return nil
	}
	return m.heldListed(projectID, c)
}

// RelistAgentReads re-dials every connected agent read entry whose listing
// is at least olderThan old, so a descriptor change between reloads is
// judged (amendment #481 item 4). RunAgentReadRelist calls it every tick;
// a test calls it with 0 to re-list everything now.
func (m *Manager) RelistAgentReads(ctx context.Context, olderThan time.Duration) {
	type entry struct {
		project, server string
		client          *Client
	}
	now := m.now()
	var due []entry
	m.mu.RLock()
	for projectID, byServer := range m.clients {
		for name, c := range byServer {
			if agentReadEntry(projectID, name) && now.Sub(c.listedAt) >= olderThan {
				due = append(due, entry{projectID, name, c})
			}
		}
	}
	m.mu.RUnlock()
	sort.Slice(due, func(i, j int) bool {
		if due[i].project != due[j].project {
			return due[i].project < due[j].project
		}
		return due[i].server < due[j].server
	})
	for _, e := range due {
		if ctx.Err() != nil {
			return
		}
		m.relistOne(e.project, e.server, e.client)
	}
}

// relistOne replaces current with a freshly listed client built from its
// own config, unless something else replaced current meanwhile.
// Serialised per (project, server) with redial and dialPending.
func (m *Manager) relistOne(projectID, serverName string, current *Client) {
	key := projectID + "\x00" + serverName
	lockAny, _ := m.redialLocks.LoadOrStore(key, &sync.Mutex{})
	lock, ok := lockAny.(*sync.Mutex)
	if !ok {
		return
	}
	lock.Lock()
	defer lock.Unlock()
	m.mu.RLock()
	stillThere := m.clients[projectID][serverName] == current
	m.mu.RUnlock()
	if !stillThere {
		return // a reload or a re-dial listed it since
	}
	ctx, cancel := context.WithTimeout(context.Background(), pendingDialTimeout)
	defer cancel()
	fresh, err := connectFn(ctx, current.config, m.logger.With().Str("project", projectID).Logger())
	if err != nil {
		agentReadRelists().WithLabelValues(projectID, serverName, "failed").Inc()
		m.logger.Warn().Err(redactConnectErr(err)).Str("project", projectID).Str("server", serverName).
			Msg("mcp: re-list of an agent read server failed; its last listing stays the verdict until it ages out")
		return
	}
	m.judgeListing(projectID, fresh)
	closer := closeFn
	m.mu.Lock()
	byServer := m.clients[projectID]
	if byServer == nil || byServer[serverName] != current {
		m.mu.Unlock()
		go func() { _ = closer(fresh) }()
		agentReadRelists().WithLabelValues(projectID, serverName, "superseded").Inc()
		return
	}
	byServer[serverName] = fresh
	m.mu.Unlock()
	agentReadRelists().WithLabelValues(projectID, serverName, "connected").Inc()
	go drainThenClose(current, drainBound(current), closer)
}

// RunAgentReadRelist re-lists agent read servers every interval until ctx
// ends, reads the store while it is unread and every interval, and retries
// unstored suspensions at every tick. The daemon runs one per node, beside the
// reconnector. While it runs, a listing older than twice the interval is
// refused at the gate.
func (m *Manager) RunAgentReadRelist(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	m.setRelistInterval(interval)
	defer m.setRelistInterval(0)
	ticker := time.NewTicker(relistTick)
	defer ticker.Stop()
	lastLoad := m.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.relistTickOnce(ctx, interval, &lastLoad)
		}
	}
}

// relistTickOnce is one tick of RunAgentReadRelist.
func (m *Manager) relistTickOnce(ctx context.Context, interval time.Duration, lastLoad *time.Time) {
	if !m.ReadHoldState().Loaded || m.now().Sub(*lastLoad) >= interval {
		if err := m.LoadReadSuspensions(ctx); err == nil {
			*lastLoad = m.now()
		}
	}
	m.retrySuspensionWrites(ctx) // a no-op unless a write failed
	m.RelistAgentReads(ctx, interval)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	relistMetricsOnce   sync.Once
	agentReadRelistsVec *prometheus.CounterVec
)

func agentReadRelists() *prometheus.CounterVec {
	relistMetricsOnce.Do(func() {
		agentReadRelistsVec = promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "mcp", Name: "agent_read_relists_total",
			Help: "Periodic re-lists of agent-namespace MCP read servers (agent-administered design §6.5, amendment #481), " +
				"by outcome: connected (installed), failed (the last listing stays), superseded (a reload or re-dial won).",
		}, []string{"project", "server", "outcome"})
	})
	return agentReadRelistsVec
}
