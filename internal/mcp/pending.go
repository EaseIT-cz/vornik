package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Failed-connect recovery (https://docs.vornik.io
// 2026-09-30-mcp-failed-connect-recovery-design.md). A project server whose
// dial failed is kept as a pending entry, retried in the background with
// backoff, dialled on use, and reported by the doctor and a notify-only
// alert. Before this, a failed dial dropped the server with its config and
// nothing retried it: ibkr-trader's broker was dark for 24 hours after a dial
// raced a container restart (2026-09-29).

const (
	reconnectInterval   = 10 * time.Second
	dialOnUseMinSpacing = 5 * time.Second
	outageAlertAfter    = 10 * time.Minute
	pendingDialTimeout  = 30 * time.Second
)

// errServerPending: the server is configured for the project but its dial
// failed and has not yet been retried successfully.
var errServerPending = errors.New("mcp server pending reconnect")

// pendingServer is one configured (project, server) without a client.
type pendingServer struct {
	cfg         ServerConfig
	lastErr     string
	since       time.Time
	lastAttempt time.Time
	nextAttempt time.Time
	lastOnUse   time.Time
	attempts    int
	alerted     bool
}

// PendingServer is the read-only view the doctor reports.
type PendingServer struct {
	ProjectID   string
	Server      string
	Since       time.Time
	LastAttempt time.Time
	Attempts    int
	LastError   string
}

// backoffAfter is the wait before the next background attempt, given how
// many dials have been made: 15 s, 30 s, 60 s, 120 s, then every 300 s.
func backoffAfter(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return 15 * time.Second
	case attempts == 2:
		return 30 * time.Second
	case attempts == 3:
		return 60 * time.Second
	case attempts == 4:
		return 120 * time.Second
	default:
		return 300 * time.Second
	}
}

func newPendingServer(cfg ServerConfig, err error, now time.Time) *pendingServer {
	p := &pendingServer{cfg: cfg, since: now}
	p.recordFailure(err, now)
	return p
}

func (p *pendingServer) recordFailure(err error, now time.Time) {
	p.attempts++
	p.lastAttempt = now
	p.nextAttempt = now.Add(backoffAfter(p.attempts))
	if err != nil {
		p.lastErr = redactConnectErr(err).Error()
	}
}

// redactConnectErr rewrites the URL inside a connect error to
// scheme://host/<redacted>: a path can carry a token (Home Assistant's
// /private_<token>), and Go's *url.Error embeds the whole URL. Host and port
// stay, because they are what an operator diagnoses with. A stdio server's
// arguments are not covered (phase 2, P4).
func redactConnectErr(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if !errors.As(err, &ue) || ue.URL == "" {
		return err
	}
	redacted := "<redacted URL>"
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		redacted = u.Scheme + "://" + u.Host + "/<redacted>"
	}
	return errors.New(strings.ReplaceAll(err.Error(), ue.URL, redacted))
}

// signalReadyLocked wakes WaitForStartingServers. Caller holds mu.
func (m *Manager) signalReadyLocked() {
	close(m.ready)
	m.ready = make(chan struct{})
}

// WaitForStartingServers waits, up to maxWait, while project has a pending
// server whose outage began less than startingWindow ago (still starting,
// not down). It re-checks after every wake, so a wake for an unrelated server
// or an entry that has aged past the window cannot keep it waiting. A Close
// during the wait wakes it; the re-check finds nothing pending and it
// returns. It returns the project's servers still starting when it stops,
// which the caller logs; each is counted on
// vornik_mcp_tools_listed_with_pending_total (phase 2, P3).
func (m *Manager) WaitForStartingServers(ctx context.Context, projectID string, maxWait, startingWindow time.Duration) []PendingServer {
	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()
	for {
		m.mu.RLock()
		now := m.now()
		starting := false
		for _, p := range m.pending[projectID] {
			if now.Sub(p.since) < startingWindow {
				starting = true
				break
			}
		}
		ready := m.ready
		m.mu.RUnlock()
		if !starting {
			return nil
		}
		select {
		case <-ready:
		case <-deadline.C:
			return m.stillStarting(projectID, startingWindow)
		case <-ctx.Done():
			// The caller went away: no step will run on this answer, so
			// nothing is counted (review-20261001-29b9).
			return nil
		}
	}
}

// stillStarting lists the project's pending servers still within the
// starting window when a wait gives up, and counts each.
func (m *Manager) stillStarting(projectID string, startingWindow time.Duration) []PendingServer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := m.now()
	var out []PendingServer
	for name, p := range m.pending[projectID] {
		if now.Sub(p.since) >= startingWindow {
			continue
		}
		out = append(out, PendingServer{ProjectID: projectID, Server: name, Since: p.since,
			LastAttempt: p.lastAttempt, Attempts: p.attempts, LastError: p.lastErr})
		toolsListedWithPending().WithLabelValues(projectID, name).Inc()
	}
	return out
}

// SetOutageNotifier wires the notify-only operator alert (Telegram in the
// daemon). Nil-safe: with no notifier nothing is sent.
func (m *Manager) SetOutageNotifier(fn func(ctx context.Context, text string) error) {
	m.mu.Lock()
	m.outageNotify = fn
	m.mu.Unlock()
}

// PendingStatus reports how many project servers the manager dialled
// (connected + pending, the doctor's denominator) and the pending ones.
func (m *Manager) PendingStatus() (examined int, pending []PendingServer) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, byServer := range m.clients {
		examined += len(byServer)
	}
	for projectID, byServer := range m.pending {
		for name, p := range byServer {
			examined++
			pending = append(pending, PendingServer{
				ProjectID: projectID, Server: name, Since: p.since,
				LastAttempt: p.lastAttempt, Attempts: p.attempts, LastError: p.lastErr,
			})
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].ProjectID != pending[j].ProjectID {
			return pending[i].ProjectID < pending[j].ProjectID
		}
		return pending[i].Server < pending[j].Server
	})
	return examined, pending
}

// pendingMessage is the model-facing text for a call against a pending
// server: what is true, and that varying the arguments will not help.
func (m *Manager) pendingMessage(projectID, serverName string) string {
	m.mu.RLock()
	p := m.pending[projectID][serverName]
	m.mu.RUnlock()
	if p == nil {
		return fmt.Sprintf("MCP server %q not connected for project %q", serverName, projectID)
	}
	return fmt.Sprintf("MCP server %q is configured for project %q but not connected (last error: %s; "+
		"retrying in the background since %s) — this is a server fault, not a problem with the "+
		"arguments, so retrying this call or varying its arguments will not help",
		serverName, projectID, p.lastErr, p.since.UTC().Format(time.RFC3339))
}

// dialPending dials one pending server, serialised per (project, server)
// with redial. onUse marks a dial made for a waiting call: it ignores the
// backoff but runs at most once per dialOnUseMinSpacing. A nil return means
// a client is installed now (by this dial or by a winner of the race).
func (m *Manager) dialPending(projectID, serverName string, onUse bool) error {
	key := projectID + "\x00" + serverName
	lockAny, _ := m.redialLocks.LoadOrStore(key, &sync.Mutex{})
	lock, ok := lockAny.(*sync.Mutex)
	if !ok {
		return fmt.Errorf("mcp: internal: bad redial lock type for %q", key)
	}
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	if m.clients[projectID][serverName] != nil {
		m.mu.Unlock()
		return nil
	}
	p := m.pending[projectID][serverName]
	if p == nil {
		m.mu.Unlock()
		return fmt.Errorf("server %q is not configured for project %q", serverName, projectID)
	}
	now := m.now()
	if onUse {
		if !p.lastOnUse.IsZero() && now.Sub(p.lastOnUse) < dialOnUseMinSpacing {
			m.mu.Unlock()
			return errServerPending
		}
		p.lastOnUse = now
	}
	gen, cfg := m.generation, p.cfg
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), pendingDialTimeout)
	defer cancel()
	client, err := connectFn(ctx, cfg, m.logger.With().Str("project", projectID).Logger())
	if err != nil {
		err = redactConnectErr(err)
		reconnectAttempts().WithLabelValues(projectID, serverName, "failed").Inc()
		m.mu.Lock()
		if m.generation == gen && m.pending[projectID][serverName] == p {
			tier := backoffAfter(p.attempts)
			p.recordFailure(err, m.now())
			next := backoffAfter(p.attempts)
			if p.attempts == 2 || next != tier || p.attempts%10 == 0 {
				m.logger.Warn().Err(err).Str("project", projectID).Str("server", serverName).
					Int("attempts", p.attempts).Dur("next_in", next).
					Msg("mcp: pending server still not reachable")
			}
		}
		m.mu.Unlock()
		return err
	}
	reconnectAttempts().WithLabelValues(projectID, serverName, "connected").Inc()
	if recovered, ok := m.installRecovered(projectID, serverName, client, gen); ok {
		m.onRecovered(projectID, serverName, recovered)
		return nil
	}
	// A reload or Close() changed the catalog while we dialled. Pinned on this
	// goroutine, as in redial.
	closer := closeFn
	go func() { _ = closer(client) }()
	return fmt.Errorf("the MCP catalog changed while re-dialling %q for project %q", serverName, projectID)
}

// installRecovered is the one pending→connected transition. It installs
// only if the catalog generation is unchanged and the entry is still
// pending, and returns the removed entry for recovery reporting.
func (m *Manager) installRecovered(projectID, serverName string, client *Client, gen uint64) (*pendingServer, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != gen {
		return nil, false
	}
	p := m.pending[projectID][serverName]
	if p == nil {
		return nil, false
	}
	if m.clients[projectID] == nil {
		m.clients[projectID] = make(map[string]*Client)
	}
	m.clients[projectID][serverName] = client
	m.dropPendingLocked(projectID, serverName)
	m.signalReadyLocked()
	return p, true
}

// dropPendingLocked removes one entry and its gauge. Caller holds mu.
func (m *Manager) dropPendingLocked(projectID, serverName string) {
	byServer := m.pending[projectID]
	if byServer == nil {
		return
	}
	delete(byServer, serverName)
	if len(byServer) == 0 {
		delete(m.pending, projectID)
	}
	pendingGauge().DeleteLabelValues(projectID, serverName)
}

// onRecovered logs the recovery and, if the outage was alerted, sends the
// recovery alert. Called outside mu.
func (m *Manager) onRecovered(projectID, serverName string, p *pendingServer) {
	outage := m.now().Sub(p.since)
	m.logger.Info().Str("project", projectID).Str("server", serverName).
		Int("attempts", p.attempts).Dur("outage", outage).
		Msg("mcp: pending server connected")
	if p.alerted {
		m.sendAlert(fmt.Sprintf("MCP server %q for project %q reconnected after %s (%d attempts).",
			serverName, projectID, outage.Round(time.Second), p.attempts+1))
	}
}

// sendAlert delivers one notify-only alert off the calling goroutine,
// bounded, and never blocks or fails the caller.
func (m *Manager) sendAlert(text string) {
	m.mu.RLock()
	notify := m.outageNotify
	m.mu.RUnlock()
	if notify == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				m.logger.Error().Interface("panic", r).Msg("mcp: outage alert panicked")
			}
		}()
		if err := notify(ctx, text); err != nil {
			m.logger.Warn().Err(err).Msg("mcp: outage alert failed")
		}
	}()
}

// reconnectPass sends due outage alerts and dials every pending entry
// whose backoff has elapsed.
func (m *Manager) reconnectPass(ctx context.Context) {
	type key struct{ project, server string }
	now := m.now()
	var due []key
	var alerts []string
	m.mu.Lock()
	for projectID, byServer := range m.pending {
		for name, p := range byServer {
			if !p.alerted && now.Sub(p.since) >= outageAlertAfter {
				p.alerted = true
				alerts = append(alerts, fmt.Sprintf("MCP server %q for project %q has not connected for %s "+
					"(last error: %s). Vornik keeps retrying; calls to its tools fail until it connects.",
					name, projectID, now.Sub(p.since).Round(time.Second), p.lastErr))
			}
			if !now.Before(p.nextAttempt) {
				due = append(due, key{projectID, name})
			}
		}
	}
	m.mu.Unlock()
	for _, text := range alerts {
		m.sendAlert(text)
	}
	for _, k := range due {
		if ctx.Err() != nil {
			return
		}
		_ = m.dialPending(k.project, k.server, false)
	}
}

// RunReconnector retries pending servers until ctx is cancelled. The daemon
// runs exactly one, from start to shutdown.
func (m *Manager) RunReconnector(ctx context.Context) {
	ticker := time.NewTicker(reconnectInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconnectPass(ctx)
		}
	}
}

// --- Prometheus surface ---

var (
	pendingMetricsOnce   sync.Once
	pendingGaugeVec      *prometheus.GaugeVec
	reconnectAttemptsVec *prometheus.CounterVec
	// toolsListedWithPendingVec counts tool lists answered without a server
	// that was still starting (phase 2, P3).
	toolsListedWithPendingVec *prometheus.CounterVec
)

func registerPendingMetrics() {
	pendingMetricsOnce.Do(func() {
		pendingGaugeVec = promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "vornik", Subsystem: "mcp", Name: "server_pending",
			Help: "1 while a project's MCP server is configured but its dial failed and it has not reconnected " +
				"(failed-connect recovery design). The series is removed when it connects or is unconfigured.",
		}, []string{"project", "server"})
		reconnectAttemptsVec = promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "mcp", Name: "reconnect_attempts_total",
			Help: "Dials of pending MCP servers by the reconnector or on use, by outcome (connected|failed).",
		}, []string{"project", "server", "outcome"})
		toolsListedWithPendingVec = promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "mcp", Name: "tools_listed_with_pending_total",
			Help: "Agent tool lists answered while a project's MCP server was still starting and had not connected " +
				"within the bounded wait: the step ran without that server's tools (failed-connect recovery, phase 2).",
		}, []string{"project", "server"})
	})
}

func toolsListedWithPending() *prometheus.CounterVec {
	registerPendingMetrics()
	return toolsListedWithPendingVec
}

func pendingGauge() *prometheus.GaugeVec {
	registerPendingMetrics()
	return pendingGaugeVec
}

func reconnectAttempts() *prometheus.CounterVec {
	registerPendingMetrics()
	return reconnectAttemptsVec
}

// isPending reports whether (project, server) is a pending entry.
func (m *Manager) isPending(projectID, serverName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pending[projectID][serverName] != nil
}
