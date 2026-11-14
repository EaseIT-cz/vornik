package dispatcher

import (
	"testing"
	"time"
)

// The web chat's "Steps" line (web-chat-design.md §2.1) uses the same
// phrases and counting as the turn status line (#642): one implementation.
func TestStepsSummary_FriendlyPhrasesWithCounts_WebChat(t *testing.T) {
	got := StepsSummary([]string{"memory_search", "memory_search", "list_tasks", "mcp__scraper__web_fetch"})
	want := "searching memory ×2 · checking your tasks · using scraper/web_fetch"
	if got != want {
		t.Errorf("StepsSummary = %q, want %q", got, want)
	}
	if StepsSummary(nil) != "" {
		t.Error("no tools, no steps line")
	}
	// The status line keeps its shape.
	p := newTurnProgress(time.Unix(0, 0))
	if line := p.noteAt("memory_search", time.Unix(0, 0)); line != "⏳ searching memory · 0s · so far: searching memory" {
		t.Errorf("status line changed: %q", line)
	}
}
