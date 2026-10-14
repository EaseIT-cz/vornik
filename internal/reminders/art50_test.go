package reminders

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: GitHub #342 row 17 — the reminder completion notifier (and
// the text-reminder runner, found while taking the inventory) called
// Channel.Send directly, so an email recipient read agent-composed content with
// no AI notice. Both now take the shared disclosure path (Art 50 design §11.4).

type namedChannel struct {
	stubChannel
	name string
}

func (n *namedChannel) Name() string { return n.name }

var publicationFooter = aidisclosure.FooterSeparator + (*aidisclosure.Service)(nil).PublicationNotice().Text

func TestArt50_ReminderCompletionOnEmailCarriesNotice(t *testing.T) {
	repo := newStubRepo()
	repo.claim = &persistence.Reminder{ID: "rem_1", Kind: persistence.ReminderKindTask, Channel: "email",
		ChannelRef: "<root@x>", Content: "Daily digest"}
	ch := &namedChannel{name: "email"}
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"email": ch}}, nil, zerolog.Nop(), time.Now)
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_1"}, true, "digest")
	if len(ch.sent) != 1 || !strings.HasSuffix(ch.sent[0], publicationFooter) {
		t.Fatalf("sent = %q, want the AI notice footer", ch.sent)
	}
}

func TestArt50_ReminderCompletionOnTelegramSendsNoticeFirst(t *testing.T) {
	repo := newStubRepo()
	repo.claim = &persistence.Reminder{ID: "rem_1", Kind: persistence.ReminderKindTask, Channel: "telegram",
		ChannelRef: "42", Content: "Daily digest"}
	ch := &namedChannel{name: "telegram"}
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), time.Now)
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_1"}, true, "digest")
	if len(ch.sent) != 1 || !strings.HasPrefix(ch.sent[0], (*aidisclosure.Service)(nil).Notice().Text+aidisclosure.FooterSeparator) ||
		!strings.Contains(ch.sent[0], "Daily digest") {
		t.Fatalf("sent = %q, want one post led by the AI notice", ch.sent)
	}
}

func TestArt50_TextReminderOnEmailCarriesNotice(t *testing.T) {
	repo := newStubRepo()
	repo.queue = []*persistence.Reminder{{ID: "rem_2", Channel: "email", ChannelRef: "<root@x>",
		Content: "call the bank", Status: persistence.ReminderStatusPending}}
	ch := &namedChannel{name: "email"}
	r := New(Config{Repo: repo, Resolver: &stubResolver{channels: map[string]conversation.Channel{"email": ch}}, Logger: zerolog.Nop()})
	r.tickOnce(context.Background())
	if len(ch.sent) != 1 || ch.sent[0] != "⏰ Reminder: call the bank"+publicationFooter {
		t.Fatalf("sent = %q, want the reminder with the AI notice footer", ch.sent)
	}
}
