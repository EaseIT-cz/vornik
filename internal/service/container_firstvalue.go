package service

// The first-value gauges' wiring (GitHub #587,
// operations-api-contract-design.md §7.2.2): one tracker, built once over the
// milestone store, handed to the recall path (api) and the answer path (the
// dispatcher), with its gauges registered on the registry /metrics serves.

import (
	"context"
	"time"

	"vornik.io/vornik/internal/firstvalue"
)

// firstValueTracker returns the container's tracker, building it on first
// use and reading the stored milestones, so a restart serves the first
// times. Nil when there is no milestone store; the tracker's methods are
// nil-safe.
func (c *Container) firstValueTracker() *firstvalue.Tracker {
	if c.firstValue != nil {
		return c.firstValue
	}
	if c.repos == nil || c.repos.InstanceMilestones == nil {
		return nil
	}
	t := firstvalue.New(c.repos.InstanceMilestones, c.Logger.With().Str("component", "firstvalue").Logger())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := t.Load(ctx); err != nil {
		c.Logger.Warn().Err(err).Msg("first value: the stored milestones could not be read; the gauges read 0 until the next event")
	}
	c.firstValue = t
	return t
}

// registerFirstValueGauges registers the gauges once, on the registry
// /metrics serves. It runs only once that registry exists: registered on
// the default registry instead, they would be invisible (the two-pass trap,
// container_http.go).
func (c *Container) registerFirstValueGauges() {
	reg := c.observabilityRegisterer()
	if reg == nil || c.firstValueRegistered {
		return
	}
	if err := c.firstValueTracker().Register(reg); err != nil {
		c.Logger.Warn().Err(err).Msg("first value: the gauges could not be registered")
		return
	}
	c.firstValueRegistered = c.firstValue != nil
}
