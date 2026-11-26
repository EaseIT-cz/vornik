package config

// GitHub #809 (agent-benchmark design, Amendment 2026-11-18 (#809), change 6):
// the bench.learning block is refused unless the daemon is a bench instance
// writing an agentbench_ database, so production cannot set it.

import (
	"strings"
	"testing"
)

func TestBench809_LearningRefusedOffABenchDaemon(t *testing.T) {
	on := BenchConfig{Instance: true, Learning: BenchLearningConfig{Auto: true}}
	for name, c := range map[string]struct {
		bench BenchConfig
		db    string
		ok    bool
	}{
		"bench daemon":            {on, "agentbench_study", true},
		"production database":     {on, "vornik", false},
		"not marked a bench":      {BenchConfig{Learning: BenchLearningConfig{Auto: true}}, "agentbench_x", false},
		"no learning keys at all": {BenchConfig{}, "vornik", true},
		"instance alone":          {BenchConfig{Instance: true}, "vornik", true},
	} {
		err := c.bench.validate(c.db, "postgres")
		if c.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), "bench.learning")) {
			t.Errorf("%s: %v, want a refusal naming bench.learning", name, err)
		}
	}
}

func TestBench809_TriggerDefaults(t *testing.T) {
	r := BenchLearningTrigger{Enabled: true}.Resolved()
	if r.Every != 5 || r.MinSamples != 5 || r.Threshold != 0.3 {
		t.Fatalf("defaults = %+v, want every 5, min_samples 5, threshold 0.3", r)
	}
}

// GitHub #890 (design §12.37): the freeze names its database by its Postgres
// OID, so a learning bench must run on Postgres.
func TestBench890_LearningNeedsPostgres(t *testing.T) {
	on := BenchConfig{Instance: true, Learning: BenchLearningConfig{Auto: true}}
	for _, driver := range []string{"postgres", ""} {
		if err := on.validate("agentbench_study", driver); err != nil {
			t.Errorf("driver %q: %v", driver, err)
		}
	}
	err := on.validate("agentbench_study", "sqlite")
	if err == nil || !strings.Contains(err.Error(), "bench.learning") || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("sqlite: %v, want a refusal naming bench.learning and postgres", err)
	}
	if err := (BenchConfig{Instance: true}).validate("agentbench_study", "sqlite"); err != nil {
		t.Fatalf("no learning keys: %v", err)
	}
}
