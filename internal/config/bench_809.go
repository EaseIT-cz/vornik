package config

// GitHub #809 (agent-benchmark design, Amendment 2026-11-18 (#809)): the
// learning study's bench-only switches. Every key under bench.learning is
// refused unless bench.instance is true and the daemon writes an agentbench_
// database (change 6), and a reload may not change the block.

import (
	"fmt"
	"strings"
)

// BenchConfig marks a benchmark daemon and carries the learning study's
// switches. Off and empty by default.
type BenchConfig struct {
	Instance bool                `yaml:"instance" doc:"This daemon is an isolated benchmark instance. Required, with an agentbench_ database, before any bench.learning key is accepted." since:"2026.11.7"`
	Learning BenchLearningConfig `yaml:"learning" doc:"The learning study's bench-only switches (GitHub #809). Refused on any daemon that is not a bench instance; not reloadable." since:"2026.11.7"`
}

// BenchLearningConfig is the bench-only learning block.
type BenchLearningConfig struct {
	Trigger BenchLearningTrigger `yaml:"trigger" doc:"The bench trigger: an absolute failure-rate trigger evaluated during the warm-up pass, for the blackbox arms." since:"2026.11.7"`
	Auto    bool                 `yaml:"auto" doc:"Run the operator's generate, replay-trial and promote steps on each bench trigger, within the shipped scorecard gate." since:"2026.11.7"`
}

// BenchLearningTrigger tunes the bench trigger. The tuning values stay in the
// arm's config hash; only Enabled is the learning axis.
type BenchLearningTrigger struct {
	Enabled    bool    `yaml:"enabled" doc:"Evaluate the bench trigger." since:"2026.11.7"`
	Every      int     `yaml:"every" doc:"Evaluate after every N completed tasks (0 = 5)." since:"2026.11.7"`
	MinSamples int     `yaml:"min_samples" doc:"Runs a workflow needs before it is evaluated (0 = 5)." since:"2026.11.7"`
	Threshold  float64 `yaml:"threshold" doc:"Failure rate at or above which a trigger is written (0 = 0.3)." since:"2026.11.7"`
}

// Resolved fills the defaults.
func (t BenchLearningTrigger) Resolved() BenchLearningTrigger {
	if t.Every <= 0 {
		t.Every = 5
	}
	if t.MinSamples <= 0 {
		t.MinSamples = 5
	}
	if t.Threshold <= 0 {
		t.Threshold = 0.3
	}
	return t
}

// Set reports whether any bench.learning key is set.
func (l BenchLearningConfig) Set() bool { return l != BenchLearningConfig{} }

// validate refuses a bench.learning block on a daemon that is not a bench
// instance writing an agentbench_ database.
func (b BenchConfig) validate(databaseName string) error {
	if !b.Learning.Set() {
		return nil
	}
	if !b.Instance || !strings.HasPrefix(databaseName, "agentbench_") {
		return fmt.Errorf("bench.learning is set, but this daemon is not a benchmark instance "+
			"(it needs bench.instance: true and a database named agentbench_*, this one is %q): "+
			"the learning study's switches never run outside a bench", databaseName)
	}
	return nil
}
