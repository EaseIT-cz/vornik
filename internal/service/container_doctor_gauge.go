package service

import "context"

// startDoctorGauge runs the doctor gauge on a managed instance (GitHub #581
// D6, enterprise-packaging-design.md amendment 2026-10-29): the read-only
// report at start and every five minutes, so the control plane alerts from
// the scrape without calling doctor. A self host keeps doctor on demand.
func (c *Container) startDoctorGauge(ctx context.Context) bool {
	if c.Config == nil || !c.Config.Hosting.Managed() || c.doctorHandlers == nil {
		return false
	}
	go c.doctorHandlers.RunDoctorGaugeLoop(ctx)
	return true
}
