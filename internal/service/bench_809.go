package service

// GitHub #809 (agent-benchmark design, Amendment 2026-11-18 (#809), the
// revision): the learning study's freeze gate. Opened only on a bench
// instance with a bench.learning block (the config guard has already
// refused one anywhere else); nil on every other daemon, and a nil gate
// always ticks.

import (
	"fmt"
	"os"
	"strings"

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
		c.benchGate, c.benchGateErr = benchlearning.Open(dir)
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
