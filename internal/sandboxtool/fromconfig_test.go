package sandboxtool

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
)

// The daemon and `vornikctl doctor` resolve sandbox_tools the same way, so
// the doctor's probes run under the daemon's limits.
func TestFromConfig(t *testing.T) {
	cfg, err := FromConfig(config.SandboxToolsConfig{
		MaxConcurrent: 3,
		Limits:        map[string]string{"audio": "3GiB"},
		CPUs:          map[string]string{"audio": " 1.5 "},
		Timeouts:      map[string]int{"audio": 900},
		MaxInputBytes: 1 << 30,
	}, "img", "/scratch")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image != "img" || cfg.ScratchRoot != "/scratch" || cfg.MaxConcurrent != 3 || cfg.MaxInputBytes != 1<<30 ||
		cfg.Limits[FeatureAudio] != 3<<30 || cfg.CPUs[FeatureAudio] != "1.5" || cfg.Timeouts[FeatureAudio] != 900*time.Second {
		t.Fatalf("got %+v", cfg)
	}
	for name, sc := range map[string]config.SandboxToolsConfig{
		"unknown feature": {Limits: map[string]string{"randr": "1GiB"}},
		"bad size":        {Limits: map[string]string{"pdf": "lots"}},
		"unbounded":       {Limits: map[string]string{"pdf": "none"}},
		"zero timeout":    {Timeouts: map[string]int{"pdf": 0}},
		"bad cpus":        {CPUs: map[string]string{"pdf": "-1"}},
		"negative pool":   {MaxConcurrent: -1},
		"negative input":  {MaxInputBytes: -1},
	} {
		if _, err := FromConfig(sc, "img", t.TempDir()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
