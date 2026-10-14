package conversation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/aidisclosure"
)

// audit 2026-10-12: GitHub #342 — notifiers and composers called Channel.Send
// directly and skipped the Art 50 notice. SendDisclosed is the shared wrapper
// (design §11.4.2).

type recSender struct {
	name string
	sent []ChannelMessage
	fail bool
}

func (r *recSender) Name() string { return r.name }
func (r *recSender) Send(_ context.Context, m ChannelMessage) (string, error) {
	if r.fail {
		return "", errors.New("down")
	}
	r.sent = append(r.sent, m)
	return "id", nil
}

type scopedSender struct{ recSender }

func (s *scopedSender) DisclosureScope(m ChannelMessage) string { return "person:" + m.SpeakerID }

type served map[string]bool

func (s served) WasServed(_ context.Context, ch, id string) (bool, error) { return s[ch+"|"+id], nil }
func (s served) MarkServed(_ context.Context, ch, id, _ string) error {
	s[ch+"|"+id] = true
	return nil
}

func TestSendDisclosed_EmailCarriesPublicationFooter(t *testing.T) {
	d := aidisclosure.New(aidisclosure.Config{}, served{})
	ch := &recSender{name: "email"}
	if _, err := SendDisclosed(context.Background(), d, ch, ChannelMessage{SessionID: "s", Text: "result"}, true); err != nil {
		t.Fatal(err)
	}
	if len(ch.sent) != 1 || !strings.HasSuffix(ch.sent[0].Text, d.PublicationNotice().Text) {
		t.Fatalf("email push must carry the notice in its body: %+v", ch.sent)
	}
}

func TestSendDisclosed_PerSessionNoticeFirstThenContent_OncePerScope(t *testing.T) {
	d := aidisclosure.New(aidisclosure.Config{}, served{})
	ch := &scopedSender{recSender{name: "slack"}}
	msg := ChannelMessage{SessionID: "T/C#1", ThreadID: "th", SpeakerID: "U1", Text: "done",
		Buttons: [][]MessageButton{{{}}}}
	for i := 0; i < 2; i++ {
		if _, err := SendDisclosed(context.Background(), d, ch, msg, true); err != nil {
			t.Fatal(err)
		}
	}
	// One post per push: an owed notice LEADS the first message (back-to-back
	// posts trip per-channel rate limits, design §11.4.2); the second push in
	// the same scope is not re-disclosed.
	if len(ch.sent) != 2 {
		t.Fatalf("want 2 posts, got %d", len(ch.sent))
	}
	first := ch.sent[0]
	if first.Text != d.Notice().Text+aidisclosure.FooterSeparator+"done" ||
		first.SessionID != "T/C#1" || first.ThreadID != "th" || len(first.Buttons) != 1 {
		t.Fatalf("first post must be notice-led content with its envelope intact: %+v", first)
	}
	if ch.sent[1].Text != "done" {
		t.Fatalf("already-disclosed scope must get the content unchanged, got %q", ch.sent[1].Text)
	}
}

func TestSendDisclosed_NoticeFailureSendsNothing(t *testing.T) {
	st := served{}
	d := aidisclosure.New(aidisclosure.Config{}, st)
	ch := &recSender{name: "telegram", fail: true}
	if _, err := SendDisclosed(context.Background(), d, ch, ChannelMessage{SessionID: "1", Text: "x"}, true); err == nil {
		t.Fatal("must fail closed")
	}
	if st["telegram|1"] {
		t.Fatal("a post that failed must leave the person still owed the notice")
	}
}
