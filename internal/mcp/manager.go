package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/ctxerr"
)

// Manager owns MCP server connections scoped per project. Every project
// that declares `mcp.servers` gets its own independent set of Client
// connections — the same server name (e.g. "gmail") in two different
// projects yields two isolated MCP processes with their own credentials
// and their own tool catalogs. That's what lets multiple operators run
// their personal assistants on the same daemon without cross-contamination.
//
// Callers must always pass a projectID. The zero string is treated as a
// lookup miss (returns no tools, rejects execute).
type Manager struct {
	mu      sync.RWMutex
	clients map[string]map[string]*Client // projectID → serverName → client
	logger  zerolog.Logger
	// redialLocks holds one mutex per (projectID, serverName) so a burst of
	// calls against a server that just died re-dials it once, not once per
	// caller. Keyed independently of `clients` because it must outlive the
	// entry it guards (the re-dial replaces that entry).
	redialLocks sync.Map
	// generation counts catalog swaps. SyncProjects bumps it under mu on every
	// swap; StartForProject snapshots it before dialling and refuses to install
	// a client built against a catalog a reload has since replaced. A plain
	// "is the project still present?" check cannot serve here the way it does
	// in redial: StartForProject is also how a project gets its FIRST clients,
	// so absence is its normal input, not evidence of removal.
	generation uint64
	// blockNotifier, when set, gets a post-hook on every successful tool
	// result to push an operator Telegram alert for a solvable scraper block
	// on a curated portal. Nil (default) → no notification. See block_notify.go.
	blockNotifier *BlockNotifier
	// pending holds each configured (project, server) whose dial failed, with
	// the config needed to retry it (failed-connect recovery design). Swapped
	// with clients by SyncProjects; guarded by mu.
	pending map[string]map[string]*pendingServer
	// withheld holds, per project and server, why a configured server is
	// deliberately not dialled (an oauth server with no grant; auth design
	// §14 G1), so a call against it says so. Guarded by mu.
	withheld map[string]map[string]string
	// now is the clock seam for backoff and alert thresholds.
	now func() time.Time
	// outageNotify sends the notify-only outage and recovery alerts; nil
	// sends nothing. Guarded by mu.
	outageNotify func(ctx context.Context, text string) error
	// ready is closed and replaced (under mu) whenever a client is installed
	// for a pending server, and on Close: the broadcast that wakes
	// WaitForStartingServers (phase 2, P3).
	ready chan struct{}
	// holds is the sticky suspension of agent read tools and the re-list
	// state (agent-administered design §6.5, amendment #481).
	holds readHolds
}

// SetBlockNotifier wires the scraper-block → Telegram notify hook. Nil-safe;
// pass nil (or never call) to leave notification off.
func (m *Manager) SetBlockNotifier(bn *BlockNotifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blockNotifier = bn
}

// NewManager creates an MCP manager. Call StartForProject once per project.
func NewManager(logger zerolog.Logger) *Manager {
	return &Manager{
		clients: make(map[string]map[string]*Client),
		pending: make(map[string]map[string]*pendingServer),
		logger:  logger,
		now:     time.Now,
		ready:   make(chan struct{}),
	}
}

// connectFn is the dial seam used by StartForProject / SyncProjects —
// overridden in tests to inject fake clients without a real transport.
var connectFn = Connect

// closeFn is the teardown seam for displaced clients — overridden in tests to
// simulate a Close() that blocks (a stdio subprocess that won't reap, an SSE
// stream that won't terminate) without a real transport.
var closeFn = (*Client).Close

// dialResult owns one attempted connection. client==nil means the dial failed:
// it is omitted from the replacement catalog and recorded as pending with cfg
// and err (failed-connect recovery design, D1).
type dialResult struct {
	projectID string
	name      string
	client    *Client
	cfg       ServerConfig
	err       error
}

// SyncProjects reconciles the manager to exactly the desired
// per-project server sets. It replaces the previous reload pattern of
// Close()-then-StartForProject, which wiped every client and then
// re-dialled with nothing connected in between — so for the duration
// of the reconnects (up to 30s per server) every in-flight and
// incoming Execute/Tools call failed with "not connected", on EVERY
// config reload, for tasks in unrelated projects too (bug-sweep
// follow-up 2026-06-04).
//
// Sequence: dial every desired client with NO lock held, swap the
// whole catalog under one write-lock acquisition (which inherently
// waits for in-flight Execute RLock holders to drain), then close the
// displaced clients. A consumer therefore always observes either the
// complete old catalog or the complete new one — never an empty
// window. Projects absent from desired are dropped; per the existing
// partial-success convention, a server that fails to dial is logged
// and skipped.
func (m *Manager) SyncProjects(ctx context.Context, desired map[string][]ServerConfig) {
	fresh := make(map[string]map[string]*Client, len(desired))
	total := 0
	for projectID, servers := range desired {
		if projectID == "" {
			m.logger.Error().Msg("mcp: SyncProjects given empty projectID — ignored")
			continue
		}
		fresh[projectID] = make(map[string]*Client, len(servers))
		total += len(servers)
	}

	// Dial every server CONCURRENTLY, each bounded at 30s. Results flow back on
	// a BUFFERED channel (cap = total) so a straggler's late send never blocks
	// and never touches a shared map — only this function's goroutine writes
	// `fresh`, so there is no data race with the swap below. Collection stops at
	// the first of: every dial reported, or `ctx` (the overall reconnect
	// budget from initMCP) expiring. That second arm is the critical one:
	// SyncProjects runs INSIDE the config-reload activator holding the
	// reloader's reloadMu, so a single dial that hangs past its own 30s ctx
	// (a transport that ignores cancellation) must not stall the whole cycle —
	// it would wedge live config reloads until a restart. We proceed with
	// whatever connected and abandon the straggler. 2026-07-08 watcher-wedge
	// root-cause fix (the initMCP-no-timeout follow-up deferred on 2026-07-06).
	results := make(chan dialResult, total)
	for projectID, servers := range desired {
		if projectID == "" {
			continue
		}
		for _, cfg := range servers {
			go func(projectID string, cfg ServerConfig) {
				connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				client, err := connectFn(connectCtx, cfg, m.logger.With().Str("project", projectID).Logger())
				if err != nil {
					status := connectFailureStatus(err) // before redaction, which may drop the type
					err = redactConnectErr(err)
					m.logger.Error().
						Err(err).
						Str("project", projectID).
						Str("server", cfg.Name).
						Int("http_status", status).
						Msg("mcp: failed to connect")
					results <- dialResult{projectID: projectID, name: cfg.Name, cfg: cfg, err: err}
					return
				}
				m.judgeListing(projectID, client) // design §6.5 item 4, amendment #481
				results <- dialResult{projectID: projectID, name: cfg.Name, client: client, cfg: cfg}
			}(projectID, cfg)
		}
	}

	now := m.now()
	freshPending, pending := m.collectDials(ctx, results, total, fresh, now)
	if pending > 0 {
		// Dials whose transports ignore cancellation can still succeed after the
		// reload budget expires. The buffered result channel keeps their send from
		// blocking, but without a receiver the resulting clients (including stdio
		// subprocesses) would be orphaned forever. Reap exactly the abandoned
		// results out of band so the reload itself remains bounded.
		// The abandoned dials are pending, recorded here where their config is
		// in scope (D1, F3).
		recordAbandonedDials(desired, fresh, freshPending, now)
	}

	m.mu.Lock()
	displaced := m.clients
	m.clients = fresh
	m.swapPendingLocked(freshPending)
	m.generation++
	gen := m.generation
	m.mu.Unlock()
	if pending > 0 {
		// A late success is installed if its entry is still pending and no
		// reload has swapped the catalog since this one (phase 2, P2);
		// otherwise it is closed, as before.
		// closeFn pinned here, on the calling goroutine (phase 1 design,
		// implementation note): the reaper outlives this call.
		go m.reapLateDialResults(results, pending, gen, closeFn)
	}

	// Close the displaced (old-catalog) clients OUT OF BAND. The new catalog is
	// already live after the swap above, so nothing needs these closes to
	// finish before SyncProjects returns — and Client.Close() CAN block (stdio
	// Close does Process.Kill + cmd.Wait; a subprocess that won't reap, or an
	// SSE stream that won't terminate, hangs it). SyncProjects runs inside the
	// config-reload activator holding the reloader's reloadMu, so a blocking
	// Close here would wedge the whole reload cycle — this, not the connect,
	// was the actual 2026-07-08 activator hang. Detach so a slow Close can't
	// hold up the reload; a pathological Close leaks one goroutine at worst.
	// Pinned on this goroutine for the same reason as in redial: these closes are
	// fire-and-forget, so reading the package seam inside them is an
	// unsynchronised read against any test that restores it.
	closer := closeFn
	for projectID, byServer := range displaced {
		for name, client := range byServer {
			go func(projectID, name string, client *Client) {
				drainThenClose(client, drainBound(client), func(c *Client) error {
					if err := closer(c); err != nil {
						m.logger.Warn().
							Err(err).
							Str("project", projectID).
							Str("server", name).
							Msg("mcp: close displaced client")
					}
					return nil
				})
			}(projectID, name, client)
		}
	}
}

// reapLateDialResults receives the dials a reconcile abandoned at its
// budget. A success is installed through installRecovered when its pending
// entry still exists and the catalog is still generation gen; anything else
// is closed. It takes no redialLocks mutex: a dial-on-use racing a late dial
// costs at most one extra dial, and the loser of the install closes its
// client (phase 2, P2).
func (m *Manager) reapLateDialResults(results <-chan dialResult, remaining int, gen uint64, closer func(*Client) error) {
	for range remaining {
		r := <-results
		if r.client == nil {
			continue
		}
		if recovered, ok := m.installRecovered(r.projectID, r.name, r.client, gen); ok {
			m.onRecovered(r.projectID, r.name, recovered)
			continue
		}
		_ = closer(r.client)
	}
}

// StartForProject connects to all MCP servers declared by a single project
// and records the resulting clients under that project's ID. Safe to call
// multiple times for the same projectID — new servers are added; existing
// server names are re-dialled (the old client is closed first).
//
// Servers that fail to connect are logged and skipped (partial success);
// the rest of the project's servers still come up.
func (m *Manager) StartForProject(ctx context.Context, projectID string, servers []ServerConfig) {
	if projectID == "" {
		m.logger.Error().Msg("mcp: StartForProject called with empty projectID — ignored")
		return
	}
	// Snapshot the catalog generation BEFORE any dial. Each dial below is
	// bounded at 30s and runs outside mu, and this call holds no reloadMu — the
	// admin refresh path (adminMCPRefresher.RefreshAll) does not take it — so a
	// config reload can complete its whole swap inside that window.
	m.mu.RLock()
	startGen := m.generation
	m.mu.RUnlock()

	for _, cfg := range servers {
		connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		client, err := connectFn(connectCtx, cfg, m.logger.With().Str("project", projectID).Logger())
		cancel()
		if err != nil {
			status := connectFailureStatus(err) // before redaction, which may drop the type
			err = redactConnectErr(err)
			m.logger.Error().
				Err(err).
				Str("project", projectID).
				Str("server", cfg.Name).
				Int("http_status", status).
				Msg("mcp: failed to connect")
			m.recordStartFailure(projectID, cfg, err, startGen)
			continue
		}
		m.judgeListing(projectID, client) // design §6.5 item 4, amendment #481
		m.mu.Lock()
		if m.generation != startGen {
			// A reload swapped the catalog while we dialled. Installing now
			// would resurrect servers the reload removed and leave them live
			// until the next one. The set we were handed is stale in full, so
			// abandon the remaining servers too — the reload dialled the
			// authoritative set itself.
			m.mu.Unlock()
			// Detached, and closeFn pinned, for the same reasons as the
			// displaced closes in SyncProjects: a stdio Close can block.
			closer := closeFn
			go func(c *Client) { _ = closer(c) }(client)
			m.logger.Info().
				Str("project", projectID).
				Str("server", cfg.Name).
				Msg("mcp: config reloaded during connect; discarding stale client")
			return
		}
		if _, ok := m.clients[projectID]; !ok {
			m.clients[projectID] = make(map[string]*Client)
		}
		// If a client with this server name already exists, close it first.
		// Otherwise the old process would leak while the new one replaces
		// its entry in the map.
		// Out of the lock and after its in-flight calls drain (GitHub #142):
		// a stdio Close is I/O (kill + wait) and must not run under m.mu.
		if old, exists := m.clients[projectID][cfg.Name]; exists {
			closer := closeFn
			go drainThenClose(old, drainBound(old), closer)
		}
		m.clients[projectID][cfg.Name] = client
		recovered := m.pending[projectID][cfg.Name]
		if recovered != nil {
			m.dropPendingLocked(projectID, cfg.Name)
			m.signalReadyLocked()
		}
		m.mu.Unlock()
		if recovered != nil {
			m.onRecovered(projectID, cfg.Name, recovered)
		}
	}
}

// recordStartFailure records a StartForProject dial failure as pending,
// unless a reload swapped the catalog meanwhile: the reload built the
// authoritative pending set itself, so this path seeds nothing (D1, F4).
func (m *Manager) recordStartFailure(projectID string, cfg ServerConfig, err error, startGen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != startGen {
		return
	}
	if existing := m.pending[projectID][cfg.Name]; existing != nil {
		existing.cfg = cfg
		existing.recordFailure(err, m.now())
		return
	}
	if m.pending[projectID] == nil {
		m.pending[projectID] = make(map[string]*pendingServer)
	}
	m.pending[projectID][cfg.Name] = newPendingServer(cfg, err, m.now())
	pendingGauge().WithLabelValues(projectID, cfg.Name).Set(1)
}

// swapPendingLocked installs a reload's pending set. A server that was
// already pending and failed again keeps its outage start, attempt count and
// alert state, so a reload neither hides an outage nor re-alerts it. Caller
// holds mu.
func (m *Manager) swapPendingLocked(fresh map[string]map[string]*pendingServer) {
	for projectID, byServer := range m.pending {
		for name, old := range byServer {
			if p := fresh[projectID][name]; p != nil {
				p.since, p.alerted = old.since, old.alerted
				p.attempts += old.attempts
				p.nextAttempt = p.lastAttempt.Add(backoffAfter(p.attempts))
				continue
			}
			pendingGauge().DeleteLabelValues(projectID, name)
		}
	}
	for projectID, byServer := range fresh {
		for name := range byServer {
			pendingGauge().WithLabelValues(projectID, name).Set(1)
		}
	}
	m.pending = fresh
}

// Tools returns the discovered tools for one project, in OpenAI
// function-calling format. Tool names are namespaced as
// mcp__{serverName}__{toolName} — the project is NOT in the qualified
// name because each project has its own independent catalog (which is
// the whole point of per-project scoping).
func (m *Manager) Tools(projectID string) []chat.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	byServer := m.clients[projectID]
	if len(byServer) == 0 {
		return nil
	}
	var tools []chat.Tool
	for serverName, client := range byServer {
		readEntry := agentReadEntry(projectID, serverName)
		for _, t := range client.Tools() {
			if readEntry && m.agentReadGate(projectID, serverName, t.Name, client) != nil {
				continue // suspended or held: not offered to a role (design §6.5 item 3, amendment #481)
			}
			qualifiedName := fmt.Sprintf("mcp__%s__%s", serverName, t.Name)

			params := t.InputSchema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			} else if stripped, ok := hideDaemonSuppliedArgs(params); ok {
				params = stripped
			}
			tools = append(tools, chat.Tool{
				Type: "function",
				Function: chat.ToolFunction{
					Name:        qualifiedName,
					Description: fmt.Sprintf("[%s] %s", serverName, t.Description),
					Parameters:  params,
				},
			})
		}
	}
	return tools
}

// Execute routes a tool call to the correct MCP server within a project.
// qualifiedName must be in mcp__{serverName}__{toolName} form. The project
// scope is enforced: a call with a server name not present in the given
// project's catalog returns an error even if another project happens to
// have a server with the same name.
func (m *Manager) Execute(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, error) {
	serverName, toolName, ok := parseQualifiedName(qualifiedName)
	if !ok {
		return "", fmt.Errorf("invalid MCP tool name: %s", qualifiedName)
	}

	// The daemon owns the caller's identity, so it supplies it rather than
	// trusting whatever the model put in the arguments.
	argsJSON = injectDaemonSuppliedArgs(argsJSON, projectID)

	start := time.Now()
	result, err := m.callOnce(ctx, projectID, serverName, toolName, argsJSON)

	// A dead stdio connection is the one failure worth retrying, because the
	// server behind it is a subprocess we can simply start again. Everything
	// else (bad argument, vendor 4xx, timeout) means the connection is healthy
	// and re-dialling would kill a working server, so the sentinel check has to
	// stay narrow.
	if errors.Is(err, ErrClientNotReading) {
		var fatal error
		result, err, fatal = m.healAndMaybeReplay(ctx, err, projectID, serverName, toolName, qualifiedName, argsJSON)
		if fatal != nil {
			return "", fatal
		}
	}
	// A server whose dial failed gets one dial on use, off every lock:
	// callOnce has returned and released its read lock (D3, F1).
	if errors.Is(err, errServerPending) {
		dialErr := m.awaitDial(ctx, func() error { return m.dialPending(projectID, serverName, true) })
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("the call ended (%w) while MCP server %q was being dialled", ctxErr, serverName)
		} else if dialErr == nil {
			result, err = m.callOnce(ctx, projectID, serverName, toolName, argsJSON)
		}
		if errors.Is(err, errServerPending) {
			err = errors.New(m.pendingMessage(projectID, serverName))
		}
	}
	duration := time.Since(start)

	if err != nil {
		m.logger.Warn().
			Err(err).
			Str("project", projectID).
			Str("server", serverName).
			Str("tool", toolName).
			Dur("duration", duration).
			Str("cause", string(ctxerr.Classify(ctx, err))).
			Msg("mcp: tool call failed")
		return "", fmt.Errorf("MCP tool %s failed: %w", qualifiedName, err)
	}

	m.logger.Info().
		Str("project", projectID).
		Str("server", serverName).
		Str("tool", toolName).
		Dur("duration", duration).
		Bool("is_error", result.IsError).
		Msg("mcp: tool call completed")

	text := result.Text()
	if result.IsError {
		return fmt.Sprintf("MCP error: %s", text), nil
	}
	// Post-hook: a solvable scraper block on a curated portal fires an
	// operator notification. Nil-safe, non-blocking, all work deferred to a
	// background worker — this must never affect the result just returned.
	m.blockNotifier.MaybeNotify(projectID, toolName, argsJSON, text)
	return text, nil
}

// healAndMaybeReplay handles a call that failed on a dead stdio connection:
// it re-dials and, when that is safe, replays the call once. A non-nil third
// return ends Execute with that error (the caller left, the call may already
// have run, or the server cannot be restarted).
func (m *Manager) healAndMaybeReplay(ctx context.Context, err error, projectID, serverName, toolName, qualifiedName, argsJSON string) (*ToolResult, error, error) {
	m.logger.Warn().
		Err(err).
		Str("project", projectID).
		Str("server", serverName).
		Msg("mcp: connection is dead — re-dialling before failing the call")
	// Decided BEFORE the redial, on the client that held the call: a
	// call the server may already have acted on is replayed only when
	// the tool cannot change state (GitHub #142 #6).
	mayHaveRun := errors.Is(err, ErrResponseLost) && m.toolMayMutate(projectID, serverName, toolName)
	redialErr := m.awaitDial(ctx, func() error { return m.redial(projectID, serverName) })
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The caller left while the server was being restarted. The dial
		// carries on in the background and heals the connection for the
		// next caller; this call is not replayed.
		return nil, nil, fmt.Errorf("MCP tool %s: the call ended (%w) while MCP server %q was being restarted; it was not repeated",
			qualifiedName, ctxErr, serverName)
	}
	if redialErr == nil && mayHaveRun {
		m.logger.Warn().
			Str("project", projectID).
			Str("server", serverName).
			Str("tool", toolName).
			Msg("mcp: re-dial succeeded, but the lost call can change state — not replaying it")
		return nil, nil, fmt.Errorf("MCP tool %s: server %q stopped after it received this call and before it answered, "+
			"so the call may have run. It was not repeated because %s can change state: check whether it took "+
			"effect before calling it again (the server has been restarted)", qualifiedName, serverName, toolName)
	}
	if redialErr != nil {
		m.logger.Error().
			Err(redialErr).
			Str("project", projectID).
			Str("server", serverName).
			Msg("mcp: re-dial failed; the server is down")
		// Say this in terms the MODEL can act on. The raw wording ("no
		// longer reading responses", "closed stdout before responding")
		// reads like a malformed call, and on 2026-08-05 the assistant
		// duly blamed its own arguments for an MCP subprocess that had
		// exited.
		return nil, nil, fmt.Errorf("MCP server %q is not running and could not be restarted (%v) — "+
			"this is a server fault, not a problem with the arguments, so retrying "+
			"this call or varying its arguments will not help", serverName, redialErr)
	}
	m.logger.Info().
		Str("project", projectID).
		Str("server", serverName).
		Msg("mcp: re-dial succeeded — retrying the tool call")
	result, err := m.callOnce(ctx, projectID, serverName, toolName, argsJSON)
	return result, err, nil
}

// awaitDial runs a dial (redial or dial-on-use) and waits for it, or for the
// caller's context, whichever ends first (GitHub #142 #6, outcome-quality
// LLD §S6). The dial itself deliberately ignores the caller's context (see
// redial): it heals the connection for whoever comes next and has its own
// 30s bound, so the goroutine it runs on is bounded too. What the caller's
// context now controls is only how long THIS caller waits for it.
func (m *Manager) awaitDial(ctx context.Context, dial func() error) error {
	done := make(chan error, 1)
	go func() { done <- dial() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// toolMayMutate reports whether a tool on this server can change state, from
// the server's advertised descriptor (annotations, then the name heuristic;
// Tool.IsMutating). A tool the catalog does not list is judged by its name
// alone, which treats anything not recognisably a read as mutating.
func (m *Manager) toolMayMutate(projectID, serverName, toolName string) bool {
	m.mu.RLock()
	client := m.clients[projectID][serverName]
	m.mu.RUnlock()
	if client != nil {
		for _, t := range client.Tools() {
			if t.Name == toolName {
				return t.IsMutating()
			}
		}
	}
	return Tool{Name: toolName}.IsMutating()
}

// callOnce resolves the client and makes exactly one tool call.
//
// The read lock covers the map lookup only, NOT the call (GitHub #142,
// mcp-transport-design "A stdio tool call is bounded, and the manager lock is
// not held across it"). It used to be held across CallTool so a displaced
// client could not be closed mid-call; but a waiting writer blocks every new
// RLock, so one call that never returned plus one reload wedged every
// project's MCP calls and the reload itself. The call is now marked in flight
// on its client UNDER the read lock — so the mark happens before any writer's
// swap — and the writers that displace a client close it through
// drainThenClose, which waits for those marks. Re-dial still happens BETWEEN
// two calls to this helper.
func (m *Manager) callOnce(ctx context.Context, projectID, serverName, toolName, argsJSON string) (*ToolResult, error) {
	m.mu.RLock()
	byServer := m.clients[projectID]
	var client *Client
	if byServer != nil {
		client = byServer[serverName]
	}
	if client == nil {
		defer m.mu.RUnlock()
		if m.pending[projectID][serverName] != nil {
			return nil, fmt.Errorf("%w: %s for project %s", errServerPending, serverName, projectID)
		}
		return nil, errors.New(m.notConnectedLocked(projectID, serverName))
	}
	// An agent project's read call is judged against the descriptor of this
	// client's own listing before anything is sent (design §6.5, GitHub
	// #413), and against the held set (amendment #481). Pure CPU on an
	// immutable listing and the holds' own lock, so it runs under the read
	// lock, on exactly the client the call is about to use.
	if err := m.agentReadGate(projectID, serverName, toolName, client); err != nil {
		m.mu.RUnlock()
		return nil, err
	}
	client.inflight.Add(1)
	m.mu.RUnlock()
	defer client.inflight.Done()
	return client.CallTool(ctx, toolName, json.RawMessage(argsJSON))
}

// redial replaces one project's dead client for one server with a fresh
// connection, reusing the config the dead client was built from.
//
// Serialised per (project, server) and double-checked, so a burst of concurrent
// calls against a server that just died produces ONE new subprocess rather than
// one per caller — the failure mode that makes naive reconnect logic worse than
// none. Losers of the race find a live client on the re-check and return
// success without dialling.
//
// The dial deliberately does NOT inherit the caller's context. A tool call may
// be moments from its deadline, and the connection should be healed for
// everyone who comes next even if THIS request gives up; the 30s bound matches
// the other dial sites, so a hung dial cannot leak indefinitely.
func (m *Manager) redial(projectID, serverName string) error {
	key := projectID + "\x00" + serverName
	lockAny, _ := m.redialLocks.LoadOrStore(key, &sync.Mutex{})
	lock, ok := lockAny.(*sync.Mutex)
	if !ok {
		return fmt.Errorf("mcp: internal: bad redial lock type for %q", key)
	}
	lock.Lock()
	defer lock.Unlock()

	m.mu.RLock()
	current := m.clients[projectID][serverName]
	m.mu.RUnlock()
	if current == nil {
		return fmt.Errorf("server %q is no longer configured for project %q", serverName, projectID)
	}
	if !current.dead.Load() {
		// Somebody else already replaced it while we waited for the lock.
		return nil
	}
	cfg := current.config

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fresh, err := connectFn(ctx, cfg, m.logger.With().Str("project", projectID).Logger())
	if err != nil {
		// Redacted at the return: Execute logs it (phase 2, P4).
		return redactConnectErr(err)
	}
	m.judgeListing(projectID, fresh) // design §6.5 item 4, amendment #481

	// Pin the closer HERE, on the calling goroutine, rather than letting the
	// out-of-band goroutines below read the package seam whenever they happen to
	// run. Two reasons, and the second is why this is production code and not a
	// test tweak:
	//
	//  1. Those goroutines are fire-and-forget and outlive this call, so reading
	//     a package-level var inside them is a read with no ordering against
	//     anything. `go test -race` reports it against the seam restore in
	//     swapDialSeams' t.Cleanup (manager_redial_test.go) — which is a real
	//     unsynchronised read, not a test artefact.
	//  2. Semantically the closer that runs should be the one in force when we
	//     DECIDED to close, not whatever is installed by the time the goroutine
	//     is scheduled.
	closer := closeFn

	m.mu.Lock()
	byServer, projectStillThere := m.clients[projectID]
	currentAtSwap, serverStillThere := byServer[serverName]
	if !projectStillThere || !serverStillThere || currentAtSwap != current {
		// The project was dropped by a reload while we dialled. Don't
		// resurrect it — and likewise do not resurrect a removed server or
		// overwrite a newer client installed by the reload.
		m.mu.Unlock()
		go func() { _ = closer(fresh) }()
		switch {
		case !projectStillThere:
			return fmt.Errorf("project %q was removed while re-dialling %q", projectID, serverName)
		case !serverStillThere:
			return fmt.Errorf("server %q was removed from project %q while re-dialling", serverName, projectID)
		default:
			// A reload already installed another client. It is live, so the
			// caller may retry its tool call against that winner.
			return nil
		}
	}
	displaced := byServer[serverName]
	byServer[serverName] = fresh
	m.mu.Unlock()

	// Out of band: stdio Close does Kill + Wait and can block on a subprocess
	// that will not reap, and this runs on a live tool-call path.
	if displaced != nil {
		go drainThenClose(displaced, drainBound(displaced), closer)
	}
	return nil
}

// ServerCount returns the total number of connected clients across all
// projects. Used by init logs and readiness checks; not for routing.
func (m *Manager) ServerCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, byServer := range m.clients {
		n += len(byServer)
	}
	return n
}

// ProjectCount returns the number of projects with at least one
// successfully-connected MCP client.
func (m *Manager) ProjectCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, byServer := range m.clients {
		if len(byServer) > 0 {
			n++
		}
	}
	return n
}

// Close shuts down every MCP client across every project.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for projectID, byServer := range m.clients {
		for name, client := range byServer {
			if err := client.Close(); err != nil {
				m.logger.Warn().
					Err(err).
					Str("project", projectID).
					Str("server", name).
					Msg("mcp: close error")
			}
		}
	}
	m.clients = make(map[string]map[string]*Client)
	m.swapPendingLocked(make(map[string]map[string]*pendingServer))
	// A reconnector or dial-on-use dial in flight must fail its install
	// check rather than resurrect a client after shutdown (D1, F5).
	m.generation++
	// Wake any WaitForStartingServers: it re-checks, finds nothing pending,
	// and returns (phase 2, P3).
	m.signalReadyLocked()
}

// parseQualifiedName splits mcp__{server}__{tool} into server and tool names.
func parseQualifiedName(name string) (server, tool string, ok bool) {
	if !strings.HasPrefix(name, "mcp__") {
		return "", "", false
	}
	rest := name[len("mcp__"):]
	parts := strings.SplitN(rest, "__", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// recordAbandonedDials records as pending every desired server that has
// neither a client nor a failed result: its dial outlived the reload budget.
func recordAbandonedDials(desired map[string][]ServerConfig, fresh map[string]map[string]*Client,
	freshPending map[string]map[string]*pendingServer, now time.Time) {
	for projectID, servers := range desired {
		if fresh[projectID] == nil {
			continue
		}
		for _, cfg := range servers {
			if fresh[projectID][cfg.Name] != nil || freshPending[projectID][cfg.Name] != nil {
				continue
			}
			if freshPending[projectID] == nil {
				freshPending[projectID] = make(map[string]*pendingServer)
			}
			freshPending[projectID][cfg.Name] = newPendingServer(cfg, errors.New("dial exceeded the reconnect budget"), now)
		}
	}
}

// collectDials receives dial results until every dial reported or ctx (the
// reload budget) expires. Connected clients go into fresh; failed dials are
// returned as pending entries. The second result is how many dials were
// abandoned.
func (m *Manager) collectDials(ctx context.Context, results <-chan dialResult, total int,
	fresh map[string]map[string]*Client, now time.Time) (map[string]map[string]*pendingServer, int) {
	freshPending := make(map[string]map[string]*pendingServer)
	addPending := func(projectID string, p *pendingServer) {
		if freshPending[projectID] == nil {
			freshPending[projectID] = make(map[string]*pendingServer)
		}
		freshPending[projectID][p.cfg.Name] = p
	}
	pending := total
collect:
	for pending > 0 {
		select {
		case r := <-results:
			if r.client != nil {
				fresh[r.projectID][r.name] = r.client
			} else {
				addPending(r.projectID, newPendingServer(r.cfg, r.err, now))
			}
			pending--
		case <-ctx.Done():
			m.logger.Warn().
				Int("connected", total-pending).
				Int("total", total).
				Msg("mcp: reconnect budget exceeded; proceeding with connected servers, slow dials abandoned")
			break collect
		}
	}
	return freshPending, pending
}

// connectFailureStatus is the HTTP status of a connect failure when it is a
// CallError carrying one, else 0. It is logged as http_status so the
// daemon-log ring maps a refused dial to a closed connection code without
// reading the error text (live-task-observation design, Change 2026-10-08 C6).
func connectFailureStatus(err error) int {
	var ce *CallError
	if errors.As(err, &ce) {
		return ce.Status
	}
	return 0
}

// drainThenClose closes a client the manager no longer routes to, once the
// calls already routed to it have returned, or after bound, whichever comes
// first. callOnce no longer holds the manager lock across a call (GitHub
// #142), so a writer that displaces a client no longer waits for its in-flight
// calls; this keeps the rule that a displaced call is not killed under it. The
// calls are themselves bounded (the stdio bound, the HTTP timeout), so a call
// still running at bound has already failed on its own timeout.
//
// Every in-flight mark was made under the manager's read lock, before the
// writer's swap removed the client, so none can arrive after the wait starts.
// Idempotent: a client displaced twice is waited on twice, and Close is
// idempotent. Runs out of band (go drainThenClose(...)).
func drainThenClose(c *Client, bound time.Duration, closer func(*Client) error) {
	drained := make(chan struct{})
	go func() {
		c.inflight.Wait()
		close(drained)
	}()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
	}
	_ = closer(c)
}

// drainBound is how long a displaced client may keep serving its in-flight
// calls before it is closed: its own request bound.
func drainBound(c *Client) time.Duration {
	if c != nil && c.stdioTimeout > 0 {
		return c.stdioTimeout
	}
	return defaultStdioTimeout
}
