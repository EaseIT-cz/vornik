package dispatcher

// GitHub #644 (dispatcher turn-context design §8.2): a turn the user stopped
// ends with "[Stopped.]", not an apology or "ask again" (review d298 F6).

import (
	"context"
	"errors"
	"testing"
)

func TestStreamFailureNotice_StoppedByUser_644(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrStoppedByUser)
	if got := streamFailureNotice(ctx, context.Canceled, true); got != "\n\n[Stopped.]" {
		t.Fatalf("partial: %q", got)
	}
	if got := streamFailureNotice(ctx, context.Canceled, false); got != "[Stopped.]" {
		t.Fatalf("no text: %q", got)
	}
	// Any other cancel keeps today's wording.
	other, cancel2 := context.WithCancelCause(context.Background())
	cancel2(errors.New("something else"))
	if got := streamFailureNotice(other, context.Canceled, false); got == "[Stopped.]" {
		t.Fatal("only a /stop reads as stopped")
	}
}
