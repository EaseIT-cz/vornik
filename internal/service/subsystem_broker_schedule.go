package service

import (
	"context"
	"errors"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/brokerschedule"
	"vornik.io/vornik/internal/leaderelection"
)

// BrokerScheduleSubsystem runs agent broker workflows on their approved
// schedules (agent-administered Vornik design §17.3). It follows
// RemindersSubsystem: its own database-backed worker elector, so one
// replica ticks; exactly one task per slot is the tasks table's unique
// idempotency index either way.
type BrokerScheduleSubsystem struct {
	logger    zerolog.Logger
	scheduler *brokerschedule.Scheduler
	elector   *leaderelection.Elector
}

// NewBrokerScheduleSubsystem builds the subsystem.
func NewBrokerScheduleSubsystem() *BrokerScheduleSubsystem { return &BrokerScheduleSubsystem{} }

// Name implements Subsystem.
func (s *BrokerScheduleSubsystem) Name() string { return "broker_schedule" }

// Build implements Subsystem: skipped unless agent administration is on,
// the node runs workers, and the API server (the firer) exists.
func (s *BrokerScheduleSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()
	switch {
	case c.Config == nil || !c.Config.AgentAdmin.IsEnabled():
		return SubsystemSkipped("agent_admin is off")
	case !c.capabilities().RunWorkers:
		return SubsystemSkipped("node profile: run_workers=false")
	case c.apiServer == nil || c.Registry == nil || c.repos == nil || c.repos.AgentGrants == nil:
		return SubsystemSkipped("the API server, registry or approval tables are not wired")
	}
	s.scheduler = brokerschedule.New(brokerschedule.Config{Source: scheduleSource{c: c}, Firer: scheduleFirer{c: c}, Logger: s.logger})
	s.elector = c.initWorkerElector(s.Name())
	return nil
}

// Start implements Subsystem.
func (s *BrokerScheduleSubsystem) Start(ctx context.Context) error {
	if s == nil || s.scheduler == nil {
		return nil
	}
	if s.elector != nil {
		s.scheduler.SetLeaderGate(s.elector)
		s.elector.BootstrapAcquire(ctx)
		go s.elector.Run(ctx)
	}
	go s.scheduler.Run(ctx)
	s.logger.Info().Msg("broker schedule started")
	return nil
}

// Stop implements Subsystem.
func (s *BrokerScheduleSubsystem) Stop(context.Context) error { return nil }

// scheduleSource lists the live registry's scheduled agent workflows. A
// schedule counts as approved exactly when the executor's own reach check
// passes for the workflow as loaded, so the scheduler and the executor
// cannot disagree.
type scheduleSource struct{ c *Container }

// Scheduled implements brokerschedule.Source.
func (s scheduleSource) Scheduled(ctx context.Context) ([]brokerschedule.Entry, error) {
	if s.c.Registry == nil {
		return nil, errors.New("no registry")
	}
	var out []brokerschedule.Entry
	for _, wf := range s.c.Registry.ListWorkflows() {
		if wf == nil || wf.Broker == nil || wf.Broker.Schedule == nil {
			continue
		}
		if _, agent := agentns.FromID(wf.ID); !agent {
			continue
		}
		e := brokerschedule.Entry{WorkflowID: wf.ID, Cron: wf.Broker.Schedule.Cron, Timezone: wf.Broker.Schedule.Timezone}
		if s.c.verifyAgentReach(ctx, agentadmin.ProjectOfWorkflow(wf.ID), wf.ID) == nil {
			if row, err := s.c.repos.AgentGrants.GetWorkflowReach(ctx, wf.ID); err == nil {
				e.Approved, e.ApprovedAt = true, row.ApprovedAt
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// scheduleFirer reads c.apiServer at fire time: initHTTPServer runs twice
// during boot and replaces it, so a server captured at Build would be the
// discarded first one.
type scheduleFirer struct{ c *Container }

// FireScheduledBroker implements brokerschedule.Firer.
func (f scheduleFirer) FireScheduledBroker(ctx context.Context, workflowID, key string) (string, error) {
	srv := f.c.apiServer
	if srv == nil {
		return "", errors.New("the API server is not running")
	}
	return srv.FireScheduledBroker(ctx, workflowID, key)
}
