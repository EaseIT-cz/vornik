package configassist

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #219 (config-assistant design §17), at the level it was found: the
// reference host's workspace PROJECT_CONTEXT.md carries a Google Calendar
// secret address as a markdown list item, and the assistant sent it to its
// model, its judge and the stored diff. It is now refused before any call.
func TestPropose_219_CalendarURLInWorkspaceContextRefuses(t *testing.T) {
	f := newFixture(t)
	ws := t.TempDir()
	ctxPath := filepath.Join(ws, "assistant", ".autonomy", "PROJECT_CONTEXT.md")
	if err := os.MkdirAll(filepath.Dir(ctxPath), 0o700); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("0a9b8c7d", 4)
	body := "# Context\n- family calendar: https://calendar.google.com/calendar/ical/x%40example.com/private-" + token + "/basic.ics\n"
	if err := os.WriteFile(ctxPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f.engine.WorkspaceRoot = ws

	res, err := f.engine.Propose(context.Background(), operatorReq("less often"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal == nil || res.Refusal.Code != RefuseSecretHygiene ||
		!strings.Contains(res.Refusal.Error(), "tokenised calendar URL") ||
		!strings.Contains(res.Refusal.Error(), "projects/assistant/PROJECT_CONTEXT.md") {
		t.Fatalf("want a hygiene refusal naming the calendar URL and the file, got %v", res.Refusal)
	}
	if strings.Contains(res.Refusal.Error(), token) {
		t.Fatal("the refusal must not carry the token")
	}
	if f.assistant.Calls() != 0 || f.judge.Calls() != 0 {
		t.Fatal("no model call may happen while the context holds a calendar token")
	}
}
