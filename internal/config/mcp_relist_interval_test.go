package config

import (
	"strings"
	"testing"
	"time"
)

// GitHub #481 (2026-10-21): suspension not sticky; no re-list between
// reloads. The re-list interval of agent read servers is configuration
// (operator decision A7, second-opinion review 95bd): 15m by default,
// bounded 1m..24h, with no "off".
func TestMCPConfig481_AgentReadRelistInterval(t *testing.T) {
	if got := (MCPConfig{}).AgentReadRelist(); got != 15*time.Minute {
		t.Fatalf("default = %s, want 15m", got)
	}
	if got := (MCPConfig{AgentReadRelistInterval: "5m"}).AgentReadRelist(); got != 5*time.Minute {
		t.Fatalf("5m = %s", got)
	}
	for _, ok := range []string{"", "1m", "15m", "24h"} {
		if err := (MCPConfig{AgentReadRelistInterval: ok}).Validate(); err != nil {
			t.Fatalf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"0", "0s", "-5m", "59s", "25h", "soon"} {
		err := (MCPConfig{AgentReadRelistInterval: bad}).Validate()
		if err == nil || !strings.Contains(err.Error(), "mcp.agent_read_relist_interval") {
			t.Fatalf("%q accepted or unnamed: %v", bad, err)
		}
		if got := (MCPConfig{AgentReadRelistInterval: bad}).AgentReadRelist(); got != 15*time.Minute {
			t.Fatalf("an unusable %q resolves to %s, want the default", bad, got)
		}
	}
}
