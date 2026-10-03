package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/brokergrants"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Standing grants (broker write-actions design, "Tier 2: standing grants"
// as revised, rounds 3 and 4, review 61a5): the container's wiring of the
// brokergrants service to the registry, the secret-store seal, the agent
// approval tables, the operator alert channel and the action worker.

// brokerGrants builds the standing-grant service over the current
// repositories, or nil without the grant store. Built per call (it holds
// only closures and the shared metrics): the Postgres path rebuilds c.repos
// between the two initHTTPServer passes, so a cached instance could hold a
// stale store.
func (c *Container) brokerGrants() *brokergrants.Service {
	if c.repos == nil || c.repos.BrokerGrants == nil || c.repos.BrokerActions == nil {
		return nil
	}
	if c.brokerGrantMetrics == nil {
		c.brokerGrantMetrics = brokergrants.NewMetrics()
	}
	return brokergrants.New(brokergrants.Config{
		Grants:  c.repos.BrokerGrants,
		Actions: c.repos.BrokerActions,
		Proposal: func(workflowID, action string) (registry.BrokerProposal, bool) {
			if c.Registry == nil {
				return registry.BrokerProposal{}, false
			}
			wf := c.Registry.GetWorkflow(workflowID)
			if wf == nil || wf.Broker == nil {
				return registry.BrokerProposal{}, false
			}
			for _, p := range wf.Broker.Proposes {
				if p.Action == action {
					return p, true
				}
			}
			return registry.BrokerProposal{}, false
		},
		ReachHash: c.standingReachHash,
		SealerForWrite: func() (brokergrants.Sealer, error) {
			st, err := c.secretStoreForWrite()
			if err != nil {
				return nil, err
			}
			return st, nil
		},
		SealerForRead: func() (brokergrants.Sealer, error) {
			src := c.currentSecretSource()
			if src.Store == nil {
				if src.KeyErr != nil {
					return nil, src.KeyErr
				}
				return nil, errors.New("no standing grant has been sealed yet")
			}
			return src.Store, nil
		},
		Bounds: func() (int, int, int) {
			d, u, l, err := c.Config.Broker.StandingGrants.Effective()
			if err != nil { // the loader refuses this; never widen on a bad value
				return 1, 1, 1
			}
			return d, u, l
		},
		Metrics: c.brokerGrantMetrics,
		Logger:  c.Logger.With().Str("component", "standing-grants").Logger(),
		Notify:  c.notifyStandingDigest,
	})
}

// standingReachHash is the reach hash a grant pins (item 6): an agent
// namespace's device-approved hash for the workflow, an operator project's
// live one.
func (c *Container) standingReachHash(ctx context.Context, projectID, workflowID string) (string, error) {
	if _, agent := agentns.FromID(projectID); agent {
		if c.repos == nil || c.repos.AgentGrants == nil {
			return "", errors.New("the agent approval tables are not available")
		}
		a, err := c.repos.AgentGrants.GetWorkflowReach(ctx, workflowID)
		if err != nil {
			return "", err
		}
		return a.ReachHash, nil
	}
	if c.Registry == nil {
		return "", errors.New("the project registry is not available")
	}
	p, wf := c.Registry.GetProject(projectID), c.Registry.GetWorkflow(workflowID)
	if p == nil || wf == nil {
		return "", fmt.Errorf("workflow %q of project %q is not loaded", workflowID, projectID)
	}
	sig, err := agentadmin.SignatureOf(p, c.Registry.GetSwarm(p.SwarmID), wf)
	if err != nil {
		return "", err
	}
	return sig.Hash(), nil
}

// coverPendingActions approves the task's pending actions a standing grant
// covers, and kicks the worker for each. It returns how many were covered;
// the rest go to the ordinary per-write approval. A failure covers nothing:
// the write waits for a person, as without tier 2.
func (c *Container) coverPendingActions(ctx context.Context, projectID, taskID string) int {
	svc := c.brokerGrants()
	if svc == nil {
		return 0
	}
	rows, err := c.repos.BrokerActions.ListByTask(ctx, taskID)
	if err != nil {
		c.Logger.Error().Err(err).Str("task_id", taskID).Msg("standing grants: the task's actions could not be listed; none covered")
		return 0
	}
	covered := 0
	for _, a := range rows {
		if a == nil || a.ProjectID != projectID || a.Status != persistence.BrokerActionPending {
			continue
		}
		ok, err := svc.Cover(ctx, a)
		if err != nil {
			c.Logger.Error().Err(err).Str("action_id", a.ActionID).Msg("standing grants: coverage failed; the write waits for a per-write approval")
			continue
		}
		if ok {
			covered++
			c.Logger.Info().Str("action_id", a.ActionID).Str("project", projectID).Msg("broker action approved under a standing grant")
			c.brokerActionWorker.Kick(a.ActionID)
		}
	}
	return covered
}

// grantOfferFor is the device page's offer under a broker_action request.
func (c *Container) grantOfferFor(ctx context.Context, r persistence.AgentApprovalRequestRow) *approverdevice.GrantOffer {
	svc := c.brokerGrants()
	if svc == nil {
		return nil
	}
	var pl actionPayload
	if json.Unmarshal(r.Rendered, &pl) != nil || pl.ActionID == "" {
		return nil
	}
	a, err := c.repos.BrokerActions.Get(ctx, pl.ActionID)
	if err != nil || a.ArgsSHA256 != pl.ArgsSHA256 || a.Status != persistence.BrokerActionPending {
		return nil
	}
	// The phone decides agent namespaces' writes only; an operator
	// project's grant is made in /inbox (review 3bed).
	if _, agent := agentns.FromID(a.ProjectID); !agent {
		return nil
	}
	o := svc.Offer(a)
	if o == nil {
		return nil
	}
	return &approverdevice.GrantOffer{Action: o.Action, Key: o.Key, Days: o.Days, Uses: o.Uses, Unreviewed: o.Unreviewed}
}

// standingDevice backs the device's Standing approvals page: every agent
// namespace's grants (the device approves for all of them).
type standingDevice struct{ c *Container }

func (c *Container) standingDevicePages() approverdevice.StandingPages { return standingDevice{c: c} }

func (s standingDevice) List(ctx context.Context) ([]approverdevice.StandingView, error) {
	svc := s.c.brokerGrants()
	if svc == nil {
		return nil, nil
	}
	views, err := svc.Views(ctx, persistence.BrokerGrantFilter{Namespace: persistence.BrokerGrantAllAgentNamespaces})
	if err != nil {
		return nil, err
	}
	out := make([]approverdevice.StandingView, 0, len(views))
	for _, v := range views {
		sv := approverdevice.StandingView{ID: v.Grant.ID, Namespace: v.Grant.Namespace, Workflow: shortWorkflow(v.Grant.WorkflowID),
			Action: strings.ReplaceAll(v.Grant.Action, "_", " "), Key: v.Key, State: v.State,
			UsesLeft: v.Grant.UsesLeft, MaxUses: v.Grant.MaxUses, ExpiresAt: v.Grant.ExpiresAt.UTC()}
		for _, a := range v.Covered {
			at := a.CreatedAt
			if a.DecidedAt != nil {
				at = *a.DecidedAt
			}
			sv.Covered = append(sv.Covered, approverdevice.StandingCovered{ActionID: a.ActionID, Status: a.Status, At: at.UTC()})
		}
		out = append(out, sv)
	}
	return out, nil
}

func (s standingDevice) Change(ctx context.Context, id, verb string) error {
	svc := s.c.brokerGrants()
	if svc == nil {
		return errors.New("standing grants are not available")
	}
	g, err := svc.Get(ctx, id)
	if err != nil {
		return err
	}
	if g.Namespace == "" {
		return errors.New("an operator project's standing approval is changed in /inbox")
	}
	return svc.Change(ctx, id, verb)
}

func shortWorkflow(id string) string {
	if i := strings.LastIndex(id, "--"); i >= 0 {
		return id[i+2:]
	}
	return id
}

// approveActionWithGrant is the broker_action effect's grant branch: the
// seed approval and the grant in one transaction. When the grant cannot be
// created (the project's bound reached since the page was shown, the
// workflow no longer declaring standing, the seal unavailable) the person's
// approval of this write still stands: it is approved alone, and the
// reason logged. A stale hash is the plain approve's outcome too.
func (c *Container) approveActionWithGrant(ctx context.Context, pl actionPayload, approver string, days, uses int) error {
	svc := c.brokerGrants()
	a, err := c.repos.BrokerActions.Get(ctx, pl.ActionID)
	if err != nil {
		return err
	}
	if svc != nil {
		_, err = svc.ApproveWithGrant(ctx, a, pl.ArgsSHA256, approver, days, uses)
		if err == nil || errors.Is(err, persistence.ErrBrokerActionNoTransition) {
			return err
		}
		c.Logger.Warn().Err(err).Str("action_id", pl.ActionID).Msg("standing grant not created; the write is approved on its own")
	}
	return c.repos.BrokerActions.Approve(ctx, pl.ActionID, pl.ArgsSHA256, approver, time.Now().UTC())
}

// notifyStandingDigest is the daily digest push (item 8): content-free, a
// count and the page's link.
func (c *Container) notifyStandingDigest(ctx context.Context, ns string, n int) {
	alert := c.OperatorAlerter()
	if alert == nil || c.Config == nil || !c.Config.OperatorAlertActive() {
		return
	}
	origin := ""
	if c.Config != nil {
		origin = strings.TrimRight(c.Config.PublicOrigin(), "/")
	}
	link := origin + "/ui/inbox"
	who := "your projects"
	if ns != "" {
		link, who = origin+"/ui/approve/standing", "your assistant ("+ns+")"
	}
	alert.NotifyOperator(ctx, "Vornik: writes sent under your standing approvals",
		fmt.Sprintf("%d write(s) by %s were sent under your standing approvals in the last day, without being shown to you first.\nReview, pause or revoke them: %s", n, who, link))
}

// startBrokerGrantTicker runs standing-grant housekeeping once a minute:
// the live and paused gauges, and the digest pass (each grant at most once
// a day; the window advance is a compare-and-set, so a cluster counts each
// covered write once).
func (c *Container) startBrokerGrantTicker(ctx context.Context) {
	if c.brokerGrants() == nil {
		return
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				svc := c.brokerGrants()
				if svc == nil {
					continue
				}
				if err := svc.RefreshGauges(ctx); err != nil {
					c.Logger.Warn().Err(err).Msg("standing grants: gauges not refreshed")
				}
				if err := svc.Digest(ctx); err != nil {
					c.Logger.Warn().Err(err).Msg("standing grants: digest pass failed")
				}
			}
		}
	}()
}
