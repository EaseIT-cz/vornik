package email

// GitHub #369 §S3 #21 (outcome-quality design, Amendment 2026-11-18 (#369),
// item 4): the parser marks an automatic message (RFC 3834), which never gets
// a failure notice, and an outbound message can carry Auto-Submitted without
// an AI mark.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

func TestAutoSubmitted369_TheParserMarksAutomaticMessages(t *testing.T) {
	base := "From: %FROM%\r\nTo: bot@example.com\r\nSubject: s\r\nMessage-ID: <m1@x>\r\n%EXTRA%Content-Type: %CT%\r\n\r\nbody\r\n"
	mk := func(from, extra, ct string) []byte {
		return []byte(strings.NewReplacer("%FROM%", from, "%EXTRA%", extra, "%CT%", ct).Replace(base))
	}
	cases := map[string]struct {
		raw  []byte
		auto bool
	}{
		"a person":                     {mk("ada@example.com", "", "text/plain"), false},
		"Auto-Submitted: no":           {mk("ada@example.com", "Auto-Submitted: no\r\n", "text/plain"), false},
		"Auto-Submitted: auto-replied": {mk("ada@example.com", "Auto-Submitted: auto-replied\r\n", "text/plain"), true},
		"a bounce report":              {mk("ada@example.com", "", "multipart/report; report-type=delivery-status; boundary=b"), true},
		"Precedence: bulk":             {mk("news@example.com", "Precedence: bulk\r\n", "text/plain"), true},
		"Precedence: list":             {mk("list@example.com", "Precedence: list\r\n", "text/plain"), true},
		"mailer-daemon":                {mk("MAILER-DAEMON@example.com", "", "text/plain"), true},
		"postmaster":                   {mk("postmaster@example.com", "", "text/plain"), true},
	}
	for name, c := range cases {
		p, err := ParseRFC5322(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.AutoSubmitted != c.auto {
			t.Errorf("%s: AutoSubmitted = %v, want %v", name, p.AutoSubmitted, c.auto)
		}
	}
}

func TestAutoSubmitted369_TheHeaderNeedsNoAIMark(t *testing.T) {
	m := OutboundMessage{From: "bot@example.com", To: "ada@example.com", Subject: "s", Body: "b", Date: time.Now(), AutoSubmitted: true}
	raw, _, err := RenderRFC5322(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Auto-Submitted: auto-generated") {
		t.Fatal("a notice without an AI mark lacks Auto-Submitted")
	}
	if strings.Contains(string(raw), "X-AI-Generated") {
		t.Fatal("X-AI-Generated was written with no mark")
	}
}

func noticeChannel369(t *testing.T, recvErr error) (*Channel, *fakeSMTP) {
	t.Helper()
	cfg := validConfig()
	cfg.IMAPClient = newFakeIMAP()
	smtp := &fakeSMTP{}
	cfg.SMTPSender = smtp
	ch, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ch.recv = &captureReceiver{err: recvErr}
	return ch, smtp
}

func TestFailureNotice369_OnlyWhenTheChannelGivesUp(t *testing.T) {
	ch, smtp := noticeChannel369(t, errors.New("model unavailable: internal detail 10.0.0.5"))
	raw := RawMessage{UID: "u1", Body: sampleEmail("m1@x", "alice@external.test", "Hello", "Please help")}
	for i := 1; i < maxDeliveryAttemptsPerMessage; i++ {
		ch.handleRawMessage(context.Background(), raw)
	}
	if n := len(smtp.snapshot()); n != 0 {
		t.Fatalf("%d notices before the final attempt, want 0", n)
	}
	ch.handleRawMessage(context.Background(), raw)
	sent := smtp.snapshot()
	if len(sent) != 1 {
		t.Fatalf("%d notices after the final attempt, want 1", len(sent))
	}
	p := string(sent[0].Payload)
	if !strings.Contains(p, "Auto-Submitted: auto-generated") || !strings.Contains(p, "send it again") || strings.Contains(p, "10.0.0.5") {
		t.Fatalf("notice payload:\n%s", p)
	}
	// The notice speaks as the assistant, so it carries the Art 50 notice
	// (gate 2026-11-18: the outbound law found it sent bare).
	if !strings.Contains(strings.ReplaceAll(p, "=\r\n", ""), "AI system") {
		t.Fatalf("the failure notice carries no AI disclosure:\n%s", p)
	}
	// The same sender gives up again within the hour: no second notice.
	raw2 := RawMessage{UID: "u2", Body: sampleEmail("m2@x", "alice@external.test", "Hello", "Again")}
	for i := 0; i < maxDeliveryAttemptsPerMessage; i++ {
		ch.handleRawMessage(context.Background(), raw2)
	}
	if n := len(smtp.snapshot()); n != 1 {
		t.Fatalf("%d notices to one sender within the hour, want 1", n)
	}
}

func TestFailureNotice369_TurnDeadlineNoticesAndAutomaticMessagesDoNot(t *testing.T) {
	deadline := fmt.Errorf("%w (limit 10m0s): %w", conversation.ErrTurnDeadline, context.DeadlineExceeded)
	ch, smtp := noticeChannel369(t, deadline)
	ch.handleRawMessage(context.Background(), RawMessage{UID: "d1", Body: sampleEmail("d1@x", "bob@external.test", "Hi", "Long thing")})
	if n := len(smtp.snapshot()); n != 1 {
		t.Fatalf("a turn deadline sent %d notices, want 1", n)
	}
	auto := []byte(strings.Replace(string(sampleEmail("d2@x", "carol@external.test", "Out of office", "Away")), "Subject:", "Auto-Submitted: auto-replied\r\nSubject:", 1))
	ch.handleRawMessage(context.Background(), RawMessage{UID: "d2", Body: auto})
	if n := len(smtp.snapshot()); n != 1 {
		t.Fatalf("an automatic message got a notice (%d sent), want none", n)
	}
}
