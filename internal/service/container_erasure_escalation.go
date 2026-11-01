package service

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/retention"
)

// The GDPR Art 12(3) escalation loop (GitHub #344 slice 5d-1c; art17 redact
// design §15.16 correction 1). Its own leader-gated loop, started whatever
// retention.enabled says: inside the opt-in retention sweeper it would never
// run on a default install, and the bound it enforces would never fire.

const (
	erasureEscalationInterval = 6 * time.Hour
	erasureEscalationDelay    = 2 * time.Minute
)

// erasureEscalationState is the loop's handles on the container.
type erasureEscalationState struct {
	cancel  context.CancelFunc
	done    chan struct{}
	elector *leaderelection.Elector
	unsent  prometheus.Gauge
	pass    *retention.ErasureEscalation
	gauged  bool
}

func (c *Container) initErasureEscalation() {
	if c.DB == nil || !c.capabilities().RunWorkers {
		return
	}
	driver := ""
	if c.backend != nil {
		driver = c.backend.Driver
	}
	repo := subjectLedgerFor(driver, c.DB)
	if repo == nil {
		return
	}
	st := &erasureEscalationState{unsent: prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vornik_erasure_escalations_unsent",
		Help: "Erasure rows held by a running task inside the Art 12(3) escalation margin whose operator alert could not be sent (GitHub #344).",
	})}
	st.pass = retention.NewErasureEscalation(repo, c.operatorAlertNotifier(),
		c.Logger.With().Str("component", "erasure-escalation").Logger())
	st.pass.SetUnsent = func(n int) { st.unsent.Set(float64(n)) }

	ctx, cancel := context.WithCancel(context.Background())
	st.cancel, st.done = cancel, make(chan struct{})
	st.elector = c.initWorkerElector("erasure_escalation")
	if st.elector != nil {
		st.elector.BootstrapAcquire(ctx)
		go st.elector.Run(ctx)
	}
	c.erasureEscalation = st
	go func() {
		defer close(st.done)
		c.runErasureEscalationLoop(ctx, st)
	}()
	c.Logger.Info().Dur("interval", erasureEscalationInterval).Msg("erasure escalation started")
}

// runErasureEscalationLoop starts once per process; the elector gates each
// tick, so a new leader runs within one interval of taking over.
func (c *Container) runErasureEscalationLoop(ctx context.Context, st *erasureEscalationState) {
	timer := time.NewTimer(erasureEscalationDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if st.elector == nil || st.elector.IsLeader() {
				got, err := st.pass.Run(ctx, time.Now().UTC())
				if err != nil {
					c.Logger.Warn().Err(err).Msg("erasure escalation partially failed")
				}
				if got.Raised > 0 || got.Unsent > 0 {
					c.Logger.Info().Int("due", got.Due).Int("raised", got.Raised).Int("unsent", got.Unsent).
						Msg("erasure escalation")
				}
			}
			timer.Reset(erasureEscalationInterval)
		}
	}
}

// registerErasureEscalationGauge puts the unsent gauge on the registry
// /metrics serves, once it exists (the two-pass trap, container_http.go).
func (c *Container) registerErasureEscalationGauge() {
	st := c.erasureEscalation
	reg := c.observabilityRegisterer()
	if st == nil || reg == nil || st.gauged {
		return
	}
	if err := reg.Register(st.unsent); err != nil {
		c.Logger.Warn().Err(err).Msg("erasure escalation: the gauge could not be registered")
		return
	}
	st.gauged = true
}

// erasureEscalationElector is the loop's elector for allElectors (doctor's
// leader-lock health and shutdown release); nil when the loop is not running.
func (c *Container) erasureEscalationElector() *leaderelection.Elector {
	if c.erasureEscalation == nil {
		return nil
	}
	return c.erasureEscalation.elector
}

func (c *Container) stopErasureEscalation(ctx context.Context) {
	st := c.erasureEscalation
	if st == nil || st.cancel == nil {
		return
	}
	st.cancel()
	select {
	case <-st.done:
	case <-ctx.Done():
		c.Logger.Warn().Msg("erasure escalation did not exit within the shutdown deadline")
	}
}
