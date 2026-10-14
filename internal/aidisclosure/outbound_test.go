package aidisclosure

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// audit 2026-10-12: GitHub #342 — six (then fifteen) delivery paths sent
// agent-authored content without the Art 50 notice because each called a
// transport directly. Disclose is the one primitive they now share; these tests
// pin its cadence and failure model against design §4 / §11.4.2.

type memStore struct {
	served   map[string]bool
	readErr  error
	writeErr error
	marks    int
}

func (m *memStore) WasServed(_ context.Context, ch, s string) (bool, error) {
	if m.readErr != nil {
		return false, m.readErr
	}
	return m.served[ch+"|"+s], nil
}

func (m *memStore) MarkServed(_ context.Context, ch, s, _ string) error {
	m.marks++
	if m.writeErr != nil {
		return m.writeErr
	}
	if m.served == nil {
		m.served = map[string]bool{}
	}
	m.served[ch+"|"+s] = true
	return nil
}

func TestWithFooter(t *testing.T) {
	n := Notice{Text: "NOTICE"}
	if got := WithFooter("body", n); got != "body"+FooterSeparator+"NOTICE" {
		t.Fatalf("footer = %q", got)
	}
	if got := WithFooter("  ", n); got != "NOTICE" {
		t.Fatalf("empty body must carry the notice alone, got %q", got)
	}
}

func TestDisclose_PerMessage_FootersAuthoredWithPublicationNotice(t *testing.T) {
	s := New(Config{}, &memStore{})
	got, err := s.Disclose(context.Background(), Outbound{Channel: "email", Scope: "x", Body: "report", Authored: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := WithFooter("report", s.PublicationNotice()); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	got, _ = s.Disclose(context.Background(), Outbound{Channel: "email", Body: "reply"})
	if want := WithFooter("reply", s.Notice()); got != want {
		t.Fatalf("reply footer: got %q want %q", got, want)
	}
}

func TestDisclose_PerSession_ServesOnceThenRecords(t *testing.T) {
	st := &memStore{}
	s := New(Config{}, st)
	var sent []string
	o := Outbound{Channel: "telegram", Scope: "42", Body: "done", SendNotice: func(_ context.Context, text string) error {
		sent = append(sent, text)
		return nil
	}}
	for i := 0; i < 2; i++ {
		got, err := s.Disclose(context.Background(), o)
		if err != nil || got != "done" {
			t.Fatalf("got %q, %v", got, err)
		}
	}
	if len(sent) != 1 || sent[0] != s.Notice().Text {
		t.Fatalf("notice sends = %q, want exactly one conversational notice", sent)
	}
}

func TestDisclose_PerSession_SendFailureFailsClosed(t *testing.T) {
	s := New(Config{}, &memStore{})
	var failed []string
	_, err := s.Disclose(context.Background(), Outbound{Channel: "slack", Scope: "a", Body: "b",
		SendNotice: func(context.Context, string) error { return errors.New("5xx") },
		Failed:     func(stage string) { failed = append(failed, stage) }})
	if err == nil {
		t.Fatal("a notice that could not be delivered must stop the content (design §4 row 1)")
	}
	if strings.Join(failed, ",") != "send" {
		t.Fatalf("failed stages = %v", failed)
	}
}

func TestDisclose_PerSession_ReadErrorServesWriteErrorContinues(t *testing.T) {
	st := &memStore{readErr: errors.New("db"), writeErr: errors.New("db")}
	s := New(Config{}, st)
	var sent int
	var failed []string
	got, err := s.Disclose(context.Background(), Outbound{Channel: "web-chat", Scope: "s", Body: "b",
		SendNotice: func(context.Context, string) error { sent++; return nil },
		Failed:     func(stage string) { failed = append(failed, stage) }})
	if err != nil || got != "b" || sent != 1 {
		t.Fatalf("got %q err %v sent %d", got, err, sent)
	}
	if strings.Join(failed, ",") != "read,write" {
		t.Fatalf("failed stages = %v", failed)
	}
}

func TestDisclose_PerSession_EmptyScopeServesWithoutRecording(t *testing.T) {
	st := &memStore{}
	s := New(Config{}, st)
	var sent int
	var failed []string
	for i := 0; i < 2; i++ {
		_, _ = s.Disclose(context.Background(), Outbound{Channel: "telegram", Body: "b",
			SendNotice: func(context.Context, string) error { sent++; return nil },
			Failed:     func(stage string) { failed = append(failed, stage) }})
	}
	if sent != 2 || st.marks != 0 {
		t.Fatalf("sent %d marks %d: an unidentified recipient is served every time and never recorded", sent, st.marks)
	}
	// Review r1 F4: a served-but-unrecorded notice is an Art 99 evidence hole and
	// must be observable.
	if strings.Join(failed, ",") != "unrecorded,unrecorded" {
		t.Fatalf("failed stages = %v, want the unrecorded serve observed", failed)
	}
}

func TestDisclose_PerSession_NoSenderFailsClosed(t *testing.T) {
	s := New(Config{}, &memStore{})
	if _, err := s.Disclose(context.Background(), Outbound{Channel: "telegram", Scope: "1", Body: "b"}); err == nil {
		t.Fatal("a per-session outbound with no way to deliver the notice must not proceed")
	}
}

func TestDisclose_NilServiceDisclosesWithDefaults(t *testing.T) {
	var s *Service
	got, err := s.Disclose(context.Background(), Outbound{Channel: "email", Body: "x", Authored: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, FooterSeparator+New(Config{}, nil).PublicationNotice().Text) {
		t.Fatalf("nil service must still disclose with the compiled default, got %q", got)
	}
	if s.PublicationNotice().Text == "" || s.Notice().Text == "" {
		t.Fatal("nil service notices must be the compiled defaults")
	}
}
