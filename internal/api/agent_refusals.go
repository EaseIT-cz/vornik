package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// The refusal ledger (agent-administered design §23.2, GitHub #172): every
// refused call made with an agent-admin key on the companion endpoint is
// counted by rule and, up to a per-namespace hourly cap, written as a row of
// who, what and which rule. Never the reason text or any input value.

// agentRefusalRowsPerHour caps the rows one namespace writes per UTC clock
// hour, counted in the table so a restart does not reset it. The doctor's
// burst check fires at 20 an hour, well below.
const agentRefusalRowsPerHour = 60

// agentRefusalWriteTimeout bounds the cap count and the write together. They
// run on a context detached from the request, so a client that disconnects
// after its refusal was answered does not lose the row (#334 audit
// 2026-10-14); the bound keeps a stuck database from pinning the handler.
const agentRefusalWriteTimeout = 5 * time.Second

// refusalUnknownTool is the verb recorded for a tool name the companion
// endpoint does not define, so a caller cannot write an arbitrary string.
const refusalUnknownTool = "unknown_tool"

var (
	agentRefusalsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Name: "agent_admin_refusals_total",
		Help: "Refused calls made with an agent-admin key on the companion endpoint, by rule (agent-administered design §23.2). Counts every refusal the agent caused, written or not; record_failed, a daemon-side failure, is not counted (§23.5).",
	}, []string{"rule"})
	agentRefusalRowsUnwritten = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Name: "agent_admin_refusal_rows_unwritten_total",
		Help: "Agent-admin refusals counted but not written to agent_admin_refusals, by reason (cap, write_failed, lock_timeout).",
	}, []string{"reason"})
)

// AgentRefusalLedger records agent-admin refusals. A nil ledger records
// nothing.
type AgentRefusalLedger struct {
	repo   persistence.AgentAdminRefusalRepository
	logger zerolog.Logger
	now    func() time.Time
	// writeTimeout bounds the lock wait, the cap count and the write
	// together (agentRefusalWriteTimeout; a test seam).
	writeTimeout time.Duration
	// mu guards nsLocks and unwritten only; it is never held across I/O.
	mu sync.Mutex
	// nsLocks serialise the cap check and the write per namespace in this
	// process, so one namespace never queues behind another (#334 audit
	// 2026-10-14; it was one process-wide lock). Namespaces are minted by
	// operators, so the map stays small.
	// Each is a one-slot semaphore so the wait for it can be bounded by the
	// write deadline (§23.8).
	nsLocks map[string]chan struct{}
	// unwritten counts, per namespace since the process started, refusals
	// counted but not written (the cap or a failed write).
	unwritten map[string]int
}

// nsLock is ns's cap-and-write lock: send to take it, receive to release.
func (l *AgentRefusalLedger) nsLock(ns string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nsLocks == nil {
		l.nsLocks = map[string]chan struct{}{}
	}
	m, ok := l.nsLocks[ns]
	if !ok {
		m = make(chan struct{}, 1)
		l.nsLocks[ns] = m
	}
	return m
}

// addUnwritten counts one refusal of ns counted but not written.
func (l *AgentRefusalLedger) addUnwritten(ns string) {
	l.mu.Lock()
	l.unwritten[ns]++
	l.mu.Unlock()
}

// NewAgentRefusalLedger builds the ledger over repo.
func NewAgentRefusalLedger(repo persistence.AgentAdminRefusalRepository, logger zerolog.Logger) *AgentRefusalLedger {
	return &AgentRefusalLedger{repo: repo, logger: logger, now: time.Now, writeTimeout: agentRefusalWriteTimeout, unwritten: map[string]int{}}
}

// WithAgentRefusalLedger wires the refusal ledger.
func WithAgentRefusalLedger(l *AgentRefusalLedger) ServerOption {
	return func(s *Server) { s.agentRefusals = l }
}

// Unwritten is how many of a namespace's refusals were counted but not
// written since this process started.
func (l *AgentRefusalLedger) Unwritten(ns string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.unwritten[ns]
}

// Record counts one refusal of key and writes its row unless the
// namespace's hourly cap is reached. Only an agent-admin key is recorded.
// The refusal is the agent's answer whatever happens here.
func (l *AgentRefusalLedger) Record(ctx context.Context, key *persistence.APIKey, verb, rule string) {
	if l == nil || key == nil || !key.AgentAdmin || key.AgentNamespace == "" {
		return
	}
	if rule == agentadmin.RuleRecordFailed {
		// A daemon-side failure to record an applied change (design §23.4),
		// not the agent's refusal: no row, no refusal count, so it never
		// feeds agent_refusal_burst (#334 audit 2026-10-14). The service
		// logged it.
		return
	}
	agentRefusalsTotal.WithLabelValues(rule).Inc()
	if l.repo == nil {
		return
	}
	ns := key.AgentNamespace
	now := l.now().UTC()
	// The deadline starts BEFORE the lock wait and bounds it too: a count
	// stuck in the database for this namespace must not hold every later
	// refusal of it without bound (delta audit 2026-10-14, §23.8). A wait
	// that expires counts the refusal as unwritten.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.writeTimeout)
	defer cancel()
	lock := l.nsLock(ns)
	select {
	case lock <- struct{}{}:
		defer func() { <-lock }()
	case <-ctx.Done():
		agentRefusalRowsUnwritten.WithLabelValues("lock_timeout").Inc()
		l.addUnwritten(ns)
		l.logger.Warn().Str("namespace", ns).Str("verb", verb).Str("rule", rule).
			Msg("agent-admin refusal ledger: namespace lock wait timed out; row not written (the refusal was answered)")
		return
	}
	if counts, err := l.repo.CountByRule(ctx, ns, now.Truncate(time.Hour)); err == nil {
		total := 0
		for _, n := range counts {
			total += n
		}
		if total >= agentRefusalRowsPerHour {
			agentRefusalRowsUnwritten.WithLabelValues("cap").Inc()
			l.addUnwritten(ns)
			return
		}
	} // a failing count writes anyway: the cap protects storage (§23.2)
	err := l.repo.Record(ctx, persistence.AgentAdminRefusal{
		ID: persistence.GenerateID("aar"), ProjectID: key.ProjectID, Namespace: ns,
		KeyID: key.ID, Verb: verb, Rule: rule, CreatedAt: now,
	})
	if err != nil {
		agentRefusalRowsUnwritten.WithLabelValues("write_failed").Inc()
		l.addUnwritten(ns)
		l.logger.Warn().Err(err).Str("namespace", ns).Str("verb", verb).Str("rule", rule).
			Msg("agent-admin refusal ledger write failed (the refusal was answered)")
	}
}

// recordAgentRefusal is the one recording helper (design §23.2): its
// callers are pinned by TestAgentRefusals_ScheduledFiringIsNotRecorded.
func (s *Server) recordAgentRefusal(ctx context.Context, key *persistence.APIKey, tool, rule string) {
	if key == nil || !key.AgentAdmin {
		return
	}
	s.agentRefusals.Record(ctx, key, refusalVerb(tool), rule)
}

// refusalVerb bounds the recorded verb to a tool the endpoint defines.
func refusalVerb(tool string) string {
	if isAgentAdminTool(tool) || tool == agentadmin.VerbApproveServerTools {
		return tool
	}
	for _, d := range companionToolDefs() {
		if d.Name == tool {
			return tool
		}
	}
	return refusalUnknownTool
}

// notDelegableError is brokerDelegable's refusal of a workflow outside an
// agent-admin key's namespace (or not an agent workflow): one answer for
// both, so the agent learns only that it cannot delegate it. A type, so the
// ledger tells it from the other refusals without matching text.
type notDelegableError struct{ workflow string }

func (e notDelegableError) Error() string {
	return fmt.Sprintf("BROKER_WORKFLOW: %q is not delegable with this key", e.workflow)
}

// delegateRefusalRule is the rule of a brokerDelegable refusal.
func delegateRefusalRule(err error) string {
	var nd notDelegableError
	if errors.As(err, &nd) {
		return agentadmin.RuleDelegateNotDelegable
	}
	return agentadmin.RuleDelegateRefused
}
