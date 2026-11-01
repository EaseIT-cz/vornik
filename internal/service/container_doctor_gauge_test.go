package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
)

// GitHub #581 D6: the gauge runs on a managed instance only, and only with a
// DB-backed doctor.
func TestStartDoctorGauge581_ManagedOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	self := &Container{Config: &config.Config{}, doctorHandlers: api.NewDoctorHandlers(nil)}
	if self.startDoctorGauge(ctx) {
		t.Error("the gauge started on a self host")
	}
	managed := &Container{Config: managedConfigForGauge()}
	if managed.startDoctorGauge(ctx) {
		t.Error("the gauge started without a doctor")
	}
	managed.doctorHandlers = api.NewDoctorHandlers(nil)
	if !managed.startDoctorGauge(ctx) {
		t.Error("the gauge did not start on a managed instance with a doctor")
	}
}

func managedConfigForGauge() *config.Config {
	cfg := &config.Config{}
	cfg.Hosting.Mode = config.HostingModeManaged
	return cfg
}
