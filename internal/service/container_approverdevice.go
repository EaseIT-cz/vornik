package service

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/approverdevice"
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
		c.approverDevices = approverdevice.New(c.repos.ApproverDevices, opts...)
	})
	return c.approverDevices
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
