package steering

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: GitHub #342 (found while taking the inventory) — the
// steering prompt carries the agent's own checkpoint question and options, and
// was sent with Channel.Send, so an email recipient read agent-authored text
// with no Art 50 notice (design §11.4.1).
func TestArt50_SteeringPromptOnEmailCarriesNotice(t *testing.T) {
	row := &persistence.ChatAuditEntry{ID: "chat_2", ChatID: "email:<thread@x.com>", UserID: "email:ops@x.com", ProjectID: "p1"}
	ch := &fakeChannel{name: "email"}
	n, em := newNotifier(t, row, ch, true)
	n.SetAIDisclosure(nil) // nil = compiled defaults

	n.NotifySteeringRequired(context.Background(),
		&persistence.Task{ID: "task_b", ProjectID: "p1", ChatTurnID: turnID("chat_2")},
		string(persistence.TaskStatusAwaitingInput))

	want := aidisclosure.FooterSeparator + (*aidisclosure.Service)(nil).PublicationNotice().Text
	if len(em.sent) != 1 || !strings.HasSuffix(em.sent[0].Text, want) {
		t.Fatalf("sent = %+v, want the AI notice footer", em.sent)
	}
}

type servedAllSteer struct{}

func (servedAllSteer) WasServed(context.Context, string, string) (bool, error)  { return true, nil }
func (servedAllSteer) MarkServed(context.Context, string, string, string) error { return nil }
