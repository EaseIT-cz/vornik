package api

// GitHub #882, retired-model design §2 R2: one HTTP 410 since daemon start is
// reported at once, in its own list, with no sample floor. ERROR while a
// loaded role or tier still resolves to the model; WARNING once none does.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

func goneErr() error {
	return &chat.GatewayError{Status: 410, Message: "minimax-m2.7 was retired on November 9, 2026."}
}

func TestModelCallsLiveGone_882_ErrorWhileARoleUsesIt(t *testing.T) {
	h, _ := profileHandlers(t) // dev-swarm: lead (complex), gate (trivial)
	tiers := map[string]config.ModelChoice{"complex": {Model: "minimax-m2.7"}}
	registry.SetDefaultModelResolution(tiers, nil)
	t.Cleanup(func() { registry.SetDefaultModelResolution(nil, nil) })
	h.agentTiers = tiers

	stats := &chat.CallStats{}
	for i := 0; i < 100; i++ {
		stats.Record("minimax-m2.7", "agent", nil)
	}
	stats.Record("minimax-m2.7", "agent", goneErr()) // 1 of 101: far below the rate
	h.callStats = stats

	c := h.checkModelCallsLive()
	text := c.Message + "\n" + strings.Join(c.Items, "\n")
	if c.Status != "ERROR" {
		t.Fatalf("status %s, want ERROR: %s", c.Status, text)
	}
	for _, want := range []string{"minimax-m2.7", "410", "retired", "dev-swarm/lead", "tier complex"} {
		if !strings.Contains(text, want) {
			t.Errorf("lacks %q: %s", want, text)
		}
	}
}

func TestModelCallsLiveGone_882_WarningWhenNoRoleUsesIt(t *testing.T) {
	h, _ := profileHandlers(t)
	stats := &chat.CallStats{}
	stats.Record("minimax-m2.7", "chat.external", goneErr())
	h.callStats = stats
	c := h.checkModelCallsLive()
	text := c.Message + "\n" + strings.Join(c.Items, "\n")
	if c.Status != "WARNING" || !strings.Contains(text, "no loaded role uses it") || !strings.Contains(text, "chat.external") {
		t.Fatalf("status %s: %s", c.Status, text)
	}
}

func TestModelCallsLiveGone_882_OwnBoundedList(t *testing.T) {
	h, _ := profileHandlers(t)
	stats := &chat.CallStats{}
	for i := 0; i < 12; i++ {
		stats.Record(fmt.Sprintf("gone-%02d", i), "agent", goneErr())
	}
	for i := 0; i < 8; i++ { // rate-flagged pairs: their own list of at most 6
		for j := 0; j < 5; j++ {
			stats.Record(fmt.Sprintf("slow-%d", i), "memory.classifier", fmt.Errorf("deadline exceeded"))
		}
	}
	h.callStats = stats
	c := h.checkModelCallsLive()
	text := c.Message + "\n" + strings.Join(c.Items, "\n")
	if got := strings.Count(text, "answered 410"); got != 10 {
		t.Errorf("%d gone lines, want 10: %s", got, text)
	}
	if !strings.Contains(text, "and 2 more retired") {
		t.Errorf("no overflow line for the gone list: %s", text)
	}
	if !strings.Contains(text, "…and 2 more pair(s) above the threshold") {
		t.Errorf("the rate list lost its own bound: %s", text)
	}
	if strings.Contains(text, "gone-00 via agent: 1/1") {
		t.Errorf("a gone pair is repeated in the rate list: %s", text)
	}
}

func TestModelCallsLiveGone_882_NoGoneKeepsTodaysBehaviour(t *testing.T) {
	h, _ := profileHandlers(t)
	stats := &chat.CallStats{}
	for i := 0; i < 5; i++ {
		stats.Record("ok-model", "agent", nil)
	}
	h.callStats = stats
	if c := h.checkModelCallsLive(); c.Status != "OK" {
		t.Fatalf("status %s: %s", c.Status, c.Message)
	}
}

// Review 20261122-237a F2: when the registry cannot load, the roles were not
// examined, so the finding stays an ERROR and says so instead of claiming no
// role uses the model.
func TestModelCallsLiveGone_882_RegistryDidNotLoad(t *testing.T) {
	h, _ := profileHandlers(t)
	if err := os.WriteFile(filepath.Join(h.configDir, "swarms", "broken.md"), []byte("---\nswarmId: [unterminated\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stats := &chat.CallStats{}
	stats.Record("minimax-m2.7", "agent", goneErr())
	h.callStats = stats
	c := h.checkModelCallsLive()
	if c.Status != "ERROR" || !strings.Contains(c.Message, "roles not examined") || strings.Contains(c.Message, "no loaded role uses it") {
		t.Fatalf("status %s: %s", c.Status, c.Message)
	}
}
