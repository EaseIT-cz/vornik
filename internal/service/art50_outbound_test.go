package service

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// audit 2026-10-12: GitHub #342 — paths that delivered agent-authored content
// by calling a transport directly, bypassing the Art 50 notice. Each test
// asserts what the RECIPIENT sees (Art 50 design §11.4).

var defaultPublication = (*aidisclosure.Service)(nil).PublicationNotice()

// alreadyDisclosed is a disclosure store for which every scope was served.
type alreadyDisclosed struct{}

func (alreadyDisclosed) WasServed(context.Context, string, string) (bool, error)  { return true, nil }
func (alreadyDisclosed) MarkServed(context.Context, string, string, string) error { return nil }

func alreadyDisclosedService() *aidisclosure.Service {
	return aidisclosure.New(aidisclosure.Config{}, alreadyDisclosed{})
}

// Row 9: the send_email tool composed fresh mail through Channel.Send and
// skipped the ChannelReceiver footer.
func TestArt50_SendEmailComposeCarriesNotice(t *testing.T) {
	fc := &fakeChannelSender{}
	sender := newEmailSender([]channelSendCloser{fc}, []*registry.Project{{ID: "p"}}, nil)
	if _, err := sender.SendEmail(context.Background(), "p", dispatcher.EmailSendRequest{
		To: "a@b.c", Subject: "s", Body: "hello"}); err != nil {
		t.Fatal(err)
	}
	calls := fc.snapshot()
	want := aidisclosure.WithFooter("hello", defaultPublication)
	if len(calls) != 1 || calls[0].Text != want {
		t.Fatalf("composed mail = %+v, want body %q", calls, want)
	}
}

type fakeEmailFileTransport struct {
	caption string
	calls   int
}

func (f *fakeEmailFileTransport) Name() string { return "email" }
func (f *fakeEmailFileTransport) SendFile(_ context.Context, _, _ string, _ []byte, caption string) (string, error) {
	f.calls++
	f.caption = caption
	return "id", nil
}

// Row 10: email SendFile (send_artifact, render_document, scheduled reports)
// mailed an AI-generated file with only the caption as body.
func TestArt50_EmailFileSendCarriesNotice(t *testing.T) {
	for _, caption := range []string{"Rendered report.pdf", ""} {
		tr := &fakeEmailFileTransport{}
		fs := emailFileSender{ch: tr, sessionID: "<root@x>"}
		if err := fs.SendArtifactFile(context.Background(), "report.pdf", strings.NewReader("pdf"), caption); err != nil {
			t.Fatal(err)
		}
		if want := aidisclosure.WithFooter(caption, defaultPublication); tr.caption != want {
			t.Fatalf("file mail body = %q, want %q", tr.caption, want)
		}
	}
}

// Row 17: the completion notifier pushed the task result with Channel.Send.
// On a per-session channel the notice must arrive first, as its own message.
func TestArt50_ChatCompletionNotifierDisclosesFirst(t *testing.T) {
	ch := &captureChannel{name: "slack"}
	n := notifierFor(ch, map[string]*persistence.ChatAuditEntry{
		"turn-1": {ID: "turn-1", ChatID: "slack:T1/C1#1.2", UserID: "slack:U1", ProjectID: "p"},
	})
	n.disclosure = nil // a chat never disclosed to; nil = compiled defaults, no store
	n.NotifyTaskCompleted(context.Background(), turnTask("task_1", "turn-1"), true, "done")
	sent := ch.snapshot()
	notice := (*aidisclosure.Service)(nil).Notice().Text
	if len(sent) != 1 || !strings.HasPrefix(sent[0].Text, notice+aidisclosure.FooterSeparator) {
		t.Fatalf("sent = %+v, want one post led by the AI notice", sent)
	}
	if !strings.Contains(sent[0].Text, "task_1") {
		t.Fatalf("the post must carry the notification after the notice, got %q", sent[0].Text)
	}
}
