//go:build integration

package integration_test

// GitHub #890 (agent-benchmark design §12.37, part 1), at the level it was
// found. Found at: production, the operator-run #809 2x2 study on 2026-11-23:
// the per-arm prepare dropped and recreated agentbench_g426b_1118 and
// restarted the daemon, but arm B's freeze file froze arm C's fresh
// database, which then learned nothing for 7.5 hours. This drives the
// daemon's own gate opener (service.Container.BenchLearningGate) against a
// real Postgres, in the integration lane's package: freeze, recreate the
// database under the same name, open again.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/service"
)

func it890Env(t *testing.T, key, def string) string {
	t.Helper()
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func it890Open(t *testing.T, dbname string) *sql.DB {
	t.Helper()
	port, _ := strconv.Atoi(it890Env(t, "POSTGRES_PORT", "5432"))
	db, err := sql.Open("postgres", fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		it890Env(t, "POSTGRES_HOST", "localhost"), port, it890Env(t, "POSTGRES_USER", "postgres"),
		os.Getenv("POSTGRES_PASSWORD"), dbname))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("postgres unreachable: %v", err)
	}
	return db
}

func benchContainer890(t *testing.T, db *sql.DB, name string) *service.Container {
	t.Helper()
	cfg := &config.Config{}
	cfg.Database.Driver, cfg.Database.Name = "postgres", name
	cfg.Bench.Instance = true
	cfg.Bench.Learning.Auto = true
	return &service.Container{Config: cfg, DB: db, Logger: zerolog.Nop()}
}

func TestIntegrationBenchFreeze_ARecreatedDatabaseIsAFreshArm_890(t *testing.T) {
	admin := it890Open(t, "postgres")
	t.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("agentbench_it890_%d", time.Now().UnixNano())
	recreate := func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
		if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + name)
	})
	t.Setenv("VORNIK_DATA_DIR", t.TempDir())

	// Arm B: open, freeze (as its measured pass does).
	recreate()
	dbB := it890Open(t, name)
	gB, err := benchContainer890(t, dbB, name).BenchLearningGate()
	if err != nil || gB == nil {
		t.Fatalf("arm B gate: %v", err)
	}
	if _, err := gB.Freeze(); err != nil {
		t.Fatal(err)
	}
	// A restart on arm B's own database stays frozen.
	gB2, err := benchContainer890(t, dbB, name).BenchLearningGate()
	if err != nil {
		t.Fatal(err)
	}
	if _, frozen := gB2.FrozenAt(); !frozen {
		t.Fatal("a restart on the same database must stay frozen")
	}
	_ = dbB.Close()

	// The prepare for arm C: the same name, recreated; the daemon restarts.
	recreate()
	dbC := it890Open(t, name)
	t.Cleanup(func() { _ = dbC.Close() })
	gC, err := benchContainer890(t, dbC, name).BenchLearningGate()
	if err != nil {
		t.Fatalf("arm C gate: %v", err)
	}
	if at, frozen := gC.FrozenAt(); frozen {
		t.Fatalf("arm C starts frozen at %s by arm B's freeze: the #890 incident", at)
	}
	entries, _ := os.ReadDir(os.Getenv("VORNIK_DATA_DIR"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !strings.Contains(strings.Join(names, " "), "bench-learning-frozen.stale-") {
		t.Fatalf("arm B's freeze must be kept for audit, data dir holds %v", names)
	}
}
