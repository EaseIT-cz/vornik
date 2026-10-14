package service

import (
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
)

// audit 2026-10-12: outcome-quality §S7 #8 (GitHub #142) — podman run and
// pull are now bounded (15m default). runtime.image_timeout is the operator's
// escape on a slow registry link (design review 0f02 #3); empty or invalid
// keeps the runtime's default.
func TestRuntimeImageTimeout_FromConfig(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want time.Duration
	}{
		{"", 0},
		{"40m", 40 * time.Minute},
		{"slow", 0},
		{"-1m", 0},
	} {
		ct := &Container{Config: &config.Config{}, Logger: zerolog.Nop()}
		ct.Config.Runtime.ImageTimeout = c.raw
		if got := ct.runtimeImageTimeout(); got != c.want {
			t.Errorf("image_timeout %q → %s, want %s", c.raw, got, c.want)
		}
	}
}
