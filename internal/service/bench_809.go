package service

// GitHub #809 (agent-benchmark design, Amendment 2026-11-18 (#809), the
// revision): the learning study's freeze gate. Opened only on a bench
// instance with a bench.learning block (the config guard has already
// refused one anywhere else); nil on every other daemon, and a nil gate
// always ticks.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"vornik.io/vornik/internal/benchlearning"
	"vornik.io/vornik/internal/config"
)

// BenchLearningGate is the freeze gate, or nil when this daemon is not a
// learning-study bench.
func (c *Container) BenchLearningGate() (*benchlearning.Gate, error) {
	if c == nil || c.Config == nil || !c.Config.Bench.Instance || !c.Config.Bench.Learning.Set() {
		return nil, nil
	}
	c.benchGateOnce.Do(func() {
		dir := strings.TrimSpace(os.Getenv("VORNIK_DATA_DIR"))
		if dir == "" {
			c.benchGateErr = fmt.Errorf("bench.learning is set but VORNIK_DATA_DIR is not: the freeze needs the bench daemon's data directory")
			return
		}
		// The freeze names its database (GitHub #890, design §12.37): a study
		// recreates the database for each arm, and a freeze from another
		// database must not freeze a fresh arm.
		id, err := benchDatabaseIdentity(context.Background(), c.DB)
		if err != nil {
			c.benchGateErr = fmt.Errorf("bench.learning: read this database's identity for the freeze: %w", err)
			return
		}
		var note string
		c.benchGate, note, c.benchGateErr = benchlearning.Open(dir, id)
		if note != "" {
			c.Logger.Warn().Str("database", id).Msg(note)
		}
	})
	return c.benchGate, c.benchGateErr
}

// benchLearningForAPI is the bench config and gate for the API routes. A gate
// that cannot open is logged and leaves the routes answering 404; the
// instinct subsystem refuses to start on the same error, so learning cannot
// run unfrozen.
func (c *Container) benchLearningForAPI() (config.BenchConfig, *benchlearning.Gate) {
	if c == nil || c.Config == nil {
		return config.BenchConfig{}, nil
	}
	g, err := c.BenchLearningGate()
	if err != nil {
		c.Logger.Error().Err(err).Msg("bench learning: the freeze gate did not open; the bench routes answer 404")
		return c.Config.Bench, nil
	}
	return c.Config.Bench, g
}

// benchDatabaseIdentity names the connected Postgres database by its name and
// OID; the OID changes when the database is dropped and recreated, even under
// the same name (GitHub #890). A learning bench runs on Postgres only
// (BenchConfig.validate).
func benchDatabaseIdentity(ctx context.Context, db *sql.DB) (string, error) {
	if db == nil {
		return "", fmt.Errorf("no database connection")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var id string
	err := db.QueryRowContext(ctx,
		`SELECT 'database ' || current_database() || ' oid ' || oid::text FROM pg_database WHERE datname = current_database()`).Scan(&id)
	return id, err
}
