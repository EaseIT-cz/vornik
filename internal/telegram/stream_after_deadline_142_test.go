package telegram

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// audit 2026-10-12 (#142 #20, outcome-quality LLD §S6 #20): the stream's edits
// rode the turn's context. When the turn ran out its time limit, the final
// edit (the one that flushes the buffered tail, and since §S6 the one that
// says the reply was cut off) was refused on the expired context, so the chat
// kept a partial preview that read like a finished answer. Edits now run on a
// detached context with their own short bound.
func TestTelegramStream_FinalEditLandsAfterTheTurnContextEnded(t *testing.T) {
	api := &telegramAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()
	bot := newTestBotWithStub(t, server)
	ch := NewChannel(bot)

	turnCtx, endTurn := context.WithCancel(context.Background())
	stream, err := ch.StreamingSend(turnCtx, "555")
	if err != nil {
		t.Fatalf("StreamingSend: %v", err)
	}
	ts := stream.(*telegramStream)
	ts.minEditEvery = 10 * time.Second
	ts.lastEdit = time.Now() // buffer the append; Close must flush it

	if err := stream.Append("partial answer"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	endTurn()
	if err := stream.Append("\n\n[cut off]"); err != nil {
		t.Fatalf("Append after the turn ended: %v", err)
	}
	if _, err := stream.Close(); err != nil {
		t.Fatalf("Close after the turn ended: %v — the preview keeps its partial text", err)
	}
	if n := api.requestCount("editMessageText"); n != 1 {
		t.Fatalf("editMessageText calls = %d, want 1 (the final flush)", n)
	}
}
