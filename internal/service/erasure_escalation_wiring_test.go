package service

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/storage"
)

// GitHub #344 5d-1c (art17 redact design §15.16 correction 1). Found at:
// review (30f3); a default install would observe it as no alert ever. The
// escalation loop must start with retention disabled, the default: inside
// the opt-in retention sweeper it never ran.
func TestErasureEscalation_StartsWithRetentionDisabled(t *testing.T) {
	db := sqlitetest.File(t, "escalation.db")
	cfg := &config.Config{}
	if cfg.Retention.Enabled {
		t.Fatal("the fixture must have retention disabled")
	}
	c := &Container{Config: cfg, DB: db.DB, backend: &storage.Backend{Driver: "sqlite"}, Logger: zerolog.Nop()}
	c.initErasureEscalation()
	if c.erasureEscalation == nil || c.erasureEscalation.pass == nil {
		t.Fatal("the escalation loop did not start with retention disabled")
	}
	c.stopErasureEscalation(context.Background())
	select {
	case <-c.erasureEscalation.done:
	default:
		t.Fatal("the loop did not stop")
	}
}
