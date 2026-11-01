package telegram

// GitHub #117, Art 50(2) step 2 commit 4, rows 11, 13 (Art 50 design §14.8
// B): a Telegram text message has no carrier, so the ledger row is the mark.
// It records the text Telegram received (after redaction and escaping), and a
// streamed reply's final text.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/voice"
)

type tgMarkLedger struct {
	mu     sync.Mutex
	hashes []string
}

func (l *tgMarkLedger) Record(_ context.Context, m persistence.AIOutputMark) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hashes = append(l.hashes, m.ContentSHA256)
	return nil
}
func (l *tgMarkLedger) RecordKey(context.Context, persistence.AIMarkKey) error { return nil }

func (l *tgMarkLedger) has(text string) bool {
	sum := sha256.Sum256([]byte(text))
	h := hex.EncodeToString(sum[:])
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.hashes {
		if x == h {
			return true
		}
	}
	return false
}

func tgMarkedBot(t *testing.T) (*Bot, *telegramAPI, *tgMarkLedger, func()) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	bot := newTestBotWithStub(t, server)
	ledger := &tgMarkLedger{}
	bot.SetAIMarker(aimark.NewMarker(aimark.NewSigner(make([]byte, 32)), ledger, func() string { return "t" }, nil, nil))
	return bot, api, ledger, server.Close
}

func TestTelegram117_SendRecordsTheTextTelegramReceived(t *testing.T) {
	bot, api, ledger, done := tgMarkedBot(t)
	defer done()
	if _, err := NewChannel(bot).Send(context.Background(), conversation.ChannelMessage{SessionID: "555", Text: "hello there"}); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	sent, _ := api.requests[len(api.requests)-1].Body["text"].(string)
	api.mu.Unlock()
	if sent == "" || !ledger.has(sent) {
		t.Fatalf("no ledger row for the text sent (%q); rows %v", sent, ledger.hashes)
	}
}

func TestTelegram117_StreamRecordsTheFinalText(t *testing.T) {
	bot, _, ledger, done := tgMarkedBot(t)
	defer done()
	stream, err := NewChannel(bot).StreamingSend(context.Background(), "555")
	if err != nil {
		t.Fatal(err)
	}
	ts := stream.(*telegramStream)
	ts.minEditEvery = 10 * time.Second
	ts.lastEdit = time.Now()
	_ = stream.Append("hello ")
	_ = stream.Append("world")
	if _, err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if !ledger.has(ts.buf) || ts.buf == "" {
		t.Fatalf("no ledger row for the streamed final text %q", ts.buf)
	}
}

// Row 12 (opt-in voice replies): the spoken reply's audio bytes get a ledger
// row, so the clip is found by its hash.
func TestTelegram117_VoiceRecordsTheAudio(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ledger := &tgMarkLedger{}
	bot.SetAIMarker(aimark.NewMarker(aimark.NewSigner(make([]byte, 32)), ledger, func() string { return "t" }, nil, nil))
	clip := []byte("OggS-voice-bytes")
	if _, err := bot.sendVoice(context.Background(), 555, voice.Audio{Bytes: clip, MimeType: "audio/ogg", DurationMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if !ledger.has(string(clip)) {
		t.Fatalf("no ledger row for the voice clip; rows %v", ledger.hashes)
	}
}
