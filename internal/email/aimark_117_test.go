package email

// GitHub #117, Art 50(2) step 2 commit 2, rows 8-10 (Art 50 design §14.8 B):
// every email the channel sends carries Auto-Submitted and a signed
// X-AI-Generated header over its body text.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

type emailMarkLedger struct {
	mu   sync.Mutex
	rows int
}

func (l *emailMarkLedger) Record(context.Context, persistence.AIOutputMark) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows++
	return nil
}
func (l *emailMarkLedger) RecordKey(context.Context, persistence.AIMarkKey) error { return nil }

func TestRenderRFC5322_117_MarkHeaders(t *testing.T) {
	m := OutboundMessage{From: "a@x", To: "b@x", Subject: "s", Body: "hi"}
	plain, _, err := RenderRFC5322(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "X-AI-Generated") || strings.Contains(string(plain), "Auto-Submitted") {
		t.Fatal("an unmarked message must carry neither header")
	}
	m.AIMark = "payload.sig"
	for _, withAttachment := range []bool{false, true} {
		if withAttachment {
			m.Attachments = []OutboundAttachment{{Filename: "f.txt", Content: []byte("x")}}
		}
		out, _, err := RenderRFC5322(m)
		if err != nil {
			t.Fatal(err)
		}
		head := strings.SplitN(string(out), "\r\n\r\n", 2)[0]
		if !strings.Contains(head, "\r\nAuto-Submitted: auto-generated\r\n") || !strings.Contains(head, "\r\nX-AI-Generated: payload.sig\r\n") {
			t.Errorf("attachment=%v: top-level headers lack the mark:\n%s", withAttachment, head)
		}
	}
}

var xAIRE = regexp.MustCompile(`X-AI-Generated: (\S+)\r\n`)

func TestChannelSend_117_MarksTheBody(t *testing.T) {
	cfg := validConfig()
	smtp := &fakeSMTP{}
	cfg.SMTPSender = smtp
	ch, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	signer := aimark.NewSigner(make([]byte, 32))
	ledger := &emailMarkLedger{}
	ch.SetAIMarker(aimark.NewMarker(signer, ledger, func() string { return "t" }, nil, nil))
	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{
		Text: "the generated reply", ChannelSpecific: map[string]string{"to": "bob@ext.test", "subject": "Hello"},
	}); err != nil {
		t.Fatal(err)
	}
	reqs := smtp.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("requests %d", len(reqs))
	}
	mm := xAIRE.FindStringSubmatch(string(reqs[0].Payload))
	if mm == nil {
		t.Fatalf("no X-AI-Generated header:\n%s", reqs[0].Payload)
	}
	mark, err := aimark.Verify(mm[1], signer.Keys())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("the generated reply"))
	if mark.SHA256 != hex.EncodeToString(sum[:]) || mark.Surface != "email" || ledger.rows != 1 {
		t.Errorf("mark %+v, ledger rows %d", mark, ledger.rows)
	}
}
