package service

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
)

// Agent-administered Vornik design §9: approver devices, the pairing page and
// the approval pages.

// approverDeviceService builds the service once. Nil when the repository is
// absent. The push goes through the operator alert channel (§9.3); without
// one, approvals still work from the page and the doctor says so.
func (c *Container) approverDeviceService() *approverdevice.Service {
	c.approverDevicesOnce.Do(func() {
		if c.repos == nil || c.repos.ApproverDevices == nil {
			return
		}
		opts := []approverdevice.Option{}
		if c.Config != nil {
			opts = append(opts, approverdevice.WithOrigin(c.Config.PublicOrigin()))
		}
		if alert := c.OperatorAlerter(); alert != nil && c.Config.OperatorAlertActive() {
			opts = append(opts, approverdevice.WithNotifier(alert.NotifyOperator))
		}
		if c.Config != nil {
			opts = append(opts, approverdevice.WithNotifyChannel(c.Config.SteeringOperatorAlert.Channel, c.Config.OperatorAlertActive()))
		}
		// Hermes approval transport design §8: vornik_host_approvals_total,
		// attached to the registry in initHTTPServer.
		if c.hostApprovalMetrics == nil {
			c.hostApprovalMetrics = approverdevice.NewHostActionMetrics()
		}
		opts = append(opts, approverdevice.WithHostActionRecorder(c.hostApprovalMetrics.Record))
		svc := approverdevice.New(c.repos.ApproverDevices, opts...)
		// A host action's page leads with its §18.7 phrase at High (design
		// §4.1). Registered here, not with the agent admin verbs: the
		// approvals arrive whether or not the admin templates are installed.
		svc.RegisterDescriber(persistence.ApprovalKindHostAction, describeHostAction)
		// Standing grants (broker write-actions design, tier 2): the offer
		// under an eligible write's Approve, and the Standing approvals
		// page. Both read the container at call time.
		svc.RegisterGrantOffer(persistence.ApprovalKindBrokerAction, c.grantOfferFor)
		svc.SetStandingPages(c.standingDevicePages())
		c.approverDevices = svc
	})
	return c.approverDevices
}

// describeHostAction is every host action's plain view (design §4.1).
func describeHostAction(persistence.AgentApprovalRequestRow) *approverdevice.Description {
	p := agentadmin.ExplainHostAction()
	return &approverdevice.Description{Summary: p.Summary, Level: p.Level, Reasons: p.Reasons}
}

// mountApproverDevicePages mounts the device routes on the OUTER mux, ahead
// of /ui/, so they never pass AuthMiddleware and no other credential is
// consulted (plan "Mounting decision"). The per-IP backstop wraps them
// outside RequireDevice (plan amendment 2), with the same limiter instance
// and budget as the main router.
func (c *Container) mountApproverDevicePages(mux *http.ServeMux) {
	svc := c.approverDeviceService()
	if svc == nil {
		return
	}
	var perIP func(http.Handler) http.Handler
	if c.Config != nil {
		perIP = api.PerIPLimit(c.perIPLimiter, c.rateLimitMetrics,
			c.Config.API.RateLimit.PerIP.RPS, c.Config.API.RateLimit.PerIP.Burst)
	}
	h := svc.Handler(perIP)
	for _, p := range approverdevice.MountPrefixes {
		mux.Handle(p, h)
	}
	c.Logger.Info().Strs("paths", approverdevice.MountPrefixes).Bool("push", svc.PushConfigured()).
		Msg("approver device pages mounted outside AuthMiddleware")
}

// startApproverDeviceTicker runs the once-a-minute housekeeping: expire
// pending requests (§12) and re-apply approved effects (plan amendment 4).
func (c *Container) startApproverDeviceTicker(ctx context.Context) {
	svc := c.approverDeviceService()
	if svc == nil {
		return
	}
	go svc.Run(ctx, time.Minute, func(err error) {
		c.Logger.Warn().Err(err).Msg("approver device housekeeping")
	})
}
