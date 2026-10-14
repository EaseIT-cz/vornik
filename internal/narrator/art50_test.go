package narrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: GitHub #342 (found while taking the inventory) — the
// milestone chat push sent LLM-composed narration with Channel.Send, so an
// email recipient read it with no Art 50 notice (design §11.4.1).
func TestArt50_ChatPushOnEmailCarriesNotice(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "email:<root@x>", ProjectID: "p1", UserID: "email:a@b.c"}
	ch := &fakeChannelCP{name: "email"}
	n, _ := newChatPushNarrator(t, task, row, ch, true)
	n.Disclosure = nil // nil = compiled defaults

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerStepCompleted,
		templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)

	want := aidisclosure.FooterSeparator + (*aidisclosure.Service)(nil).PublicationNotice().Text
	if len(ch.sent) != 1 || !strings.HasSuffix(ch.sent[0].Text, want) {
		t.Fatalf("sent = %+v, want the AI notice footer", ch.sent)
	}
}

type servedAllCP struct{}

func (servedAllCP) WasServed(context.Context, string, string) (bool, error)  { return true, nil }
func (servedAllCP) MarkServed(context.Context, string, string, string) error { return nil }
