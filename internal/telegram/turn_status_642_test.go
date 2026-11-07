package telegram

// GitHub #642 (conversation-channel design, update 2026-11-05). Found at:
// production, 2026-11-04: tool status lines piled up in the streamed answer.
// The status now sits above the streaming text and Close removes it.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

func (a *telegramAPI) editTexts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.requests {
		if s, ok := r.Body["text"].(string); ok && strings.Contains(r.Path, "editMessageText") {
			out = append(out, s)
		}
	}
	return out
}

func TestTelegramTurnStatus_AboveTextThenRemoved_642(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)
	var _ conversation.TurnStatusChannel = ch

	stream, err := ch.StreamingSend(context.Background(), "555")
	if err != nil {
		t.Fatal(err)
	}
	stream.(*telegramStream).minEditEvery = 0

	// The reporter may resolve a different Channel value for the same bot.
	NewChannel(bot).SetTurnStatus(context.Background(), "555", "⏳ list tasks ×2")
	if err := stream.Append("partial"); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	edits := api.editTexts()
	if len(edits) < 3 {
		t.Fatalf("edits = %q, want status, status+text, final", edits)
	}
	if edits[0] != "⏳ list tasks ×2" {
		t.Fatalf("status alone while no text yet: %q", edits[0])
	}
	if edits[1] != "⏳ list tasks ×2\n\npartial" {
		t.Fatalf("status above the text: %q", edits[1])
	}
	if last := edits[len(edits)-1]; last != "partial" {
		t.Fatalf("Close must leave the text alone, got %q", last)
	}
	// After Close the chat has no open stream: a late status does nothing.
	before := api.requestCount("editMessageText")
	ch.SetTurnStatus(context.Background(), "555", "late")
	time.Sleep(10 * time.Millisecond)
	if api.requestCount("editMessageText") != before {
		t.Fatal("a status after Close must not edit the finished answer")
	}
}

func TestTelegramTurnStatus_NoOpenStream_642(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	NewChannel(bot).SetTurnStatus(context.Background(), "555", "⏳ working")
	if n := len(api.requests); n != 0 {
		t.Fatalf("no open stream: no request, got %d", n)
	}
}

// A turn that ends with no text must not leave the status on the message
// (review c257 F2).
func TestTelegramTurnStatus_CloseWithNoTextRestoresPlaceholder_642(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)
	stream, err := ch.StreamingSend(context.Background(), "555")
	if err != nil {
		t.Fatal(err)
	}
	stream.(*telegramStream).minEditEvery = 0
	ch.SetTurnStatus(context.Background(), "555", "⏳ working")
	if _, err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	edits := api.editTexts()
	if len(edits) != 2 || edits[1] != telegramStreamPlaceholder {
		t.Fatalf("edits = %q, want the status then the placeholder", edits)
	}
}

// A replaced stream's Close does not remove its successor (review c257 F1).
func TestTelegramTurnStatus_ReplacedStreamKeepsSuccessor_642(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)
	first, _ := ch.StreamingSend(context.Background(), "555")
	second, _ := ch.StreamingSend(context.Background(), "555")
	if _, err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if bot.openStreams.get(555) != second.(*telegramStream) {
		t.Fatal("the older stream's Close removed the newer one")
	}
	_, _ = second.Close()
	if bot.openStreams.get(555) != nil {
		t.Fatal("Close must remove its own entry")
	}
}

// A failed status edit is terminal like a failed Append: the next Append
// returns the error, so the receiver falls back to one Send (review eb73 F1).
func TestTelegramTurnStatus_FailedStatusEditIsTerminal_642(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)
	stream, err := ch.StreamingSend(context.Background(), "555")
	if err != nil {
		t.Fatal(err)
	}
	stream.(*telegramStream).minEditEvery = 0
	// Take the API away so the status edit fails (as the existing
	// AppendEditFails test does; a single 500 is retried by the bot).
	server.Close()
	ch.SetTurnStatus(context.Background(), "555", "⏳ working")
	if err := stream.Append("text"); err == nil {
		t.Fatal("Append after a failed status edit must return the terminal error")
	}
}
