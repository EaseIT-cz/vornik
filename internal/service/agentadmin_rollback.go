package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
)

// The rollback of an agent-admin change outlives the request, and one that
// fails is counted and recorded for the doctor (agent-administered design
// §23.9, GitHub #373).

const (
	// agentRollbackTimeout bounds a compensating rollback, the lock wait
	// included. It must outlast one whole engine rollback: a deadline that
	// lands after the files are restored but before MarkRolledBack leaves the
	// files rolled back and the proposal APPLIED. A rollback took ~22 s on a
	// loaded test host (full go test ./..., 2026-10-17), so 10 s was too short.
	agentRollbackTimeout = 60 * time.Second
	// agentRollbackLockWait bounds only the wait for another apply to release
	// the config writer lock; past it the next ErrApplyInProgress is final.
	agentRollbackLockWait = 10 * time.Second
	// agentRollbackRetry is the wait between attempts while another apply
	// holds the config writer lock (ApplyEngine.Rollback uses TryLock).
	agentRollbackRetry = 200 * time.Millisecond
	// agentCompensationWriteTimeout bounds a compensating database write (the
	// REJECTED status of a failed apply, the rollback-failed audit row).
	agentCompensationWriteTimeout = 5 * time.Second
	// agentRollbackFailedAction is the admin_audit action of a failed
	// rollback; agent_change_stays_applied reads it.
	agentRollbackFailedAction = "agent_change.rollback_failed"
	// agentRollbackDoctorWindow and agentRollbackDoctorPage bound what
	// agent_change_stays_applied examines.
	agentRollbackDoctorWindow = 30 * 24 * time.Hour
	agentRollbackDoctorPage   = 100
	// rollbackErrBytes caps each error string in the audit row.
	rollbackErrBytes = 300
)

// Rollback stages: the record after a verified apply failed (R1), or the
// change did not load (R2).
const (
	rollbackStageRecord = "record"
	rollbackStageVerify = "verify"
)

// agentAdminRollbacks counts compensating rollbacks of agent-admin changes.
// outcome="failed" is a change that stays applied although the agent was told
// it was refused.
var agentAdminRollbacks = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "vornik", Name: "agent_admin_rollbacks_total",
	Help: "Compensating rollbacks of agent-admin changes, by stage (record: its record failed; verify: it did not load) and outcome (rolled_back, failed). failed means the change stays applied (agent-administered design §23.9).",
}, []string{"stage", "outcome"})

// compensationContext is ctx detached from its cancellation and bounded: a
// compensation must finish when the client that started the change has gone.
func compensationContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}

// rollBackAgentChange rolls back an applied agent change on a detached,
// bounded context, retrying while another apply holds the writer lock. A
// failure is counted and recorded durably for the doctor; the error is
// returned for the caller's answer.
func (s *agentAdminService) rollBackAgentChange(ctx context.Context, ns, id, verb, stage string, cause error) error {
	rollback := s.rollback
	if rollback == nil {
		rollback = s.engine.Rollback
	}
	rctx, cancel := compensationContext(ctx, agentRollbackTimeout)
	defer cancel()
	waitUntil := time.Now().Add(agentRollbackLockWait)
	err := rollback(rctx, id)
	for errors.Is(err, controlplane.ErrApplyInProgress) && time.Now().Before(waitUntil) {
		t := time.NewTimer(agentRollbackRetry)
		select {
		case <-rctx.Done():
			t.Stop()
			// The joined error still matches ErrApplyInProgress, so the
			// loop condition alone would spin until waitUntil: break here.
			err = errors.Join(err, rctx.Err())
		case <-t.C:
			err = rollback(rctx, id)
			continue
		}
		break
	}
	if err == nil {
		agentAdminRollbacks.WithLabelValues(stage, "rolled_back").Inc()
		return nil
	}
	agentAdminRollbacks.WithLabelValues(stage, "failed").Inc()
	s.recordRollbackFailed(ctx, ns, id, verb, stage, cause, err)
	return err
}

// recordRollbackFailed writes the durable row agent_change_stays_applied
// reads, on its own detached context: the rollback's may be spent.
func (s *agentAdminService) recordRollbackFailed(ctx context.Context, ns, id, verb, stage string, cause, rerr error) {
	if s.c.repos == nil || s.c.repos.AdminAudit == nil {
		s.c.Logger.Error().Str("proposal_id", id).Msg("agent admin: no audit repository; the doctor will not see this change that stays applied")
		return
	}
	after, _ := json.Marshal(map[string]string{
		"verb": verb, "stage": stage,
		"cause": truncate(errString(cause), rollbackErrBytes), "rollback_error": truncate(errString(rerr), rollbackErrBytes),
	})
	wctx, cancel := compensationContext(ctx, agentCompensationWriteTimeout)
	defer cancel()
	if err := s.c.repos.AdminAudit.Insert(wctx, &persistence.AdminAuditEntry{
		ID: persistence.GenerateID("admaud"), Timestamp: time.Now().UTC(),
		Principal: "agent:" + ns, Source: "daemon", Action: agentRollbackFailedAction, Target: id, After: string(after),
	}); err != nil {
		s.c.Logger.Error().Err(err).Str("proposal_id", id).
			Msg("agent admin: the record of a change that stays applied could not be written; the doctor will not see it")
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// agentChangesStayingApplied is agent_change_stays_applied's source: the
// failed-rollback rows of the window, each judged by its proposal's status
// now.
func (c *Container) agentChangesStayingApplied(ctx context.Context) (api.AgentRollbackFailures, error) {
	var out api.AgentRollbackFailures
	rows, err := c.repos.AdminAudit.List(ctx, persistence.AdminAuditFilter{
		Action: agentRollbackFailedAction, Since: time.Now().Add(-agentRollbackDoctorWindow), PageSize: agentRollbackDoctorPage,
	})
	if err != nil {
		return out, err
	}
	out.Examined = len(rows)
	out.Full = len(rows) == agentRollbackDoctorPage
	for _, r := range rows {
		p, err := c.repos.Proposals.GetByID(ctx, r.Target)
		if err != nil {
			out.Unknown = append(out.Unknown, r.Target)
			continue
		}
		if p.Status != persistence.ProposalStatusApplied {
			out.Resolved++
			continue
		}
		var after struct{ Verb, Stage string }
		_ = json.Unmarshal([]byte(r.After), &after)
		out.StillApplied = append(out.StillApplied, api.StaysAppliedChange{
			ProposalID: r.Target, Namespace: strings.TrimPrefix(r.Principal, "agent:"), Verb: after.Verb, Stage: after.Stage,
		})
	}
	return out, nil
}
