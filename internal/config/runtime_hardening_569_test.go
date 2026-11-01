package config

import (
	"strings"
	"testing"
)

// GitHub #569 (08-runtime-manager-podman.md, "Change 2026-10-28", R1): the
// agent container's read-only root and PID limit are off by default
// self-hosted, forced on (2048) in managed hosting, and a managed config that
// explicitly turns them off or sets an out-of-range limit is refused.

func TestRuntimeHardening569_Effective(t *testing.T) {
	self := baseValidConfig()
	if ro, pids := self.Runtime.AgentHardening(self.Hosting); ro || pids != 0 {
		t.Errorf("self-hosted default: read-only %v, pids %d, want off and 0", ro, pids)
	}
	on := true
	self.Runtime.AgentReadOnlyRoot = &on
	self.Runtime.AgentPidsLimit = 4096
	if ro, pids := self.Runtime.AgentHardening(self.Hosting); !ro || pids != 4096 {
		t.Errorf("self-hosted opt-in: read-only %v, pids %d", ro, pids)
	}
	m := managedConfig()
	if ro, pids := m.Runtime.AgentHardening(m.Hosting); !ro || pids != ManagedAgentPidsLimit {
		t.Errorf("managed default: read-only %v, pids %d, want on and %d", ro, pids, ManagedAgentPidsLimit)
	}
	m.Runtime.AgentPidsLimit = 4096
	if _, pids := m.Runtime.AgentHardening(m.Hosting); pids != ManagedAgentPidsLimit {
		t.Errorf("managed: an in-range explicit limit took effect (%d)", pids)
	}
}

func TestRuntimeHardening569_ManagedRefusals(t *testing.T) {
	off := false
	cases := map[string]func(*Config){
		"read-only off": func(c *Config) { c.Runtime.AgentReadOnlyRoot = &off },
		"pids too low":  func(c *Config) { c.Runtime.AgentPidsLimit = 63 },
		"pids too high": func(c *Config) { c.Runtime.AgentPidsLimit = 32769 },
		"pids negative": func(c *Config) { c.Runtime.AgentPidsLimit = -1 },
	}
	for name, mut := range cases {
		c := managedConfig()
		mut(c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "runtime.agent_") {
			t.Errorf("%s: err = %v, want a runtime.agent_ refusal", name, err)
		}
	}
	for _, ok := range []int{0, 64, 2048, 32768} {
		c := managedConfig()
		c.Runtime.AgentPidsLimit = ok
		if err := c.Validate(); err != nil {
			t.Errorf("managed pids %d refused: %v", ok, err)
		}
	}
	// Self-hosted, a negative limit is refused too; 0 means podman's default.
	s := baseValidConfig()
	s.Runtime.AgentPidsLimit = -5
	if err := s.Validate(); err == nil {
		t.Error("self-hosted: a negative agent_pids_limit was accepted")
	}
}
