package sandboxtool

import (
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/runtime"
)

// FromConfig turns the sandbox_tools block into the runner's config
// (process-spawn law S5a). The daemon and `vornikctl doctor` both build their
// runner through it, so the doctor's probes run with the daemon's limits.
// Every error is fatal at daemon startup: a limit that fell back to a
// default, or to unbounded, on a typo would leave the operator believing a
// bound is in force that is not.
func FromConfig(sc config.SandboxToolsConfig, image, scratchRoot string) (Config, error) {
	cfg := Config{
		Image:         image,
		MaxConcurrent: sc.MaxConcurrent,
		MaxInputBytes: sc.MaxInputBytes,
		ScratchRoot:   scratchRoot,
	}
	if sc.MaxConcurrent < 0 {
		return cfg, fmt.Errorf("sandbox_tools.max_concurrent %d: must be positive", sc.MaxConcurrent)
	}
	if sc.MaxInputBytes < 0 {
		return cfg, fmt.Errorf("sandbox_tools.max_input_bytes %d: must be positive", sc.MaxInputBytes)
	}
	if len(sc.Limits) > 0 {
		cfg.Limits = map[Feature]int64{}
	}
	for name, raw := range sc.Limits {
		n, err := runtime.ParseMemoryLimit(raw)
		if err != nil {
			return cfg, fmt.Errorf("sandbox_tools.limits.%s: %w", name, err)
		}
		if n == 0 {
			// "none" is the agent side's escape hatch; a sandbox run parsing
			// untrusted input must never be unbounded.
			return cfg, fmt.Errorf("sandbox_tools.limits.%s %q: a sandbox run cannot be unbounded; give a size", name, raw)
		}
		cfg.Limits[Feature(name)] = n
	}
	if len(sc.CPUs) > 0 {
		cfg.CPUs = map[Feature]string{}
	}
	for name, cpus := range sc.CPUs {
		cfg.CPUs[Feature(name)] = strings.TrimSpace(cpus)
	}
	if len(sc.Timeouts) > 0 {
		cfg.Timeouts = map[Feature]time.Duration{}
	}
	for name, secs := range sc.Timeouts {
		if secs <= 0 {
			return cfg, fmt.Errorf("sandbox_tools.timeouts.%s %d: must be a positive number of seconds", name, secs)
		}
		cfg.Timeouts[Feature(name)] = time.Duration(secs) * time.Second
	}
	// Unknown feature keys are refused by New; validate here too so the
	// startup memory check never sizes a feature that does not exist.
	if _, err := New(cfg, nil); err != nil {
		return cfg, err
	}
	return cfg, nil
}
