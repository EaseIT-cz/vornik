package telegram

// GitHub #645 (dispatcher turn-context design §9). Found at: production,
// 2026-11-04: corrections sent during a turn waited behind it.

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/dispatcher"
)

func TestSteer_MessageDuringTurnJoinsIt_645(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &stopAwareReceiver{started: make(chan string, 4)}
	bot.SetReceiver(r)
	ctx := context.Background()
	_ = bot.HandleMessage(ctx, &Message{ID: 1, ChatID: 100, UserID: 42, Text: "research X"})
	waitStarted(t, r)

	_ = bot.HandleMessage(ctx, &Message{ID: 2, ChatID: 100, UserID: 42, Text: "actually do Y"})
	bot.stops.mu.Lock()
	inbox := bot.stops.running[100].inbox
	bot.stops.mu.Unlock()
	if got := inbox.Drain(); len(got) != 1 || got[0] != "actually do Y" {
		t.Fatalf("the running turn's inbox = %v", got)
	}
	select {
	case s := <-r.started:
		t.Fatalf("a steered message must not start a turn, got %q", s)
	case <-time.After(150 * time.Millisecond):
	}
	_ = bot.handleStopCmd(ctx, 100, 42, []string{"/stop"})
}

func TestSteer_FileMessageStillQueues_645(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	_, done := bot.beginTurn(100, false, 42)
	defer done()
	if bot.offerSteer(context.Background(), &Message{ChatID: 100, UserID: 42, Text: "see file", FileID: "f1"}) {
		t.Fatal("a message with a file must take the normal turn path")
	}
	if !bot.offerSteer(context.Background(), &Message{ChatID: 100, UserID: 42, Text: "plain"}) {
		t.Fatal("a plain message must be taken by the running turn")
	}
}

func TestSteer_FollowupDeliveryDoesNotSteer_645(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	_, done := bot.beginTurn(100, true, 0)
	defer done()
	if bot.offerSteer(context.Background(), &Message{ChatID: 100, UserID: 42, Text: "plain"}) {
		t.Fatal("a follow-up delivery has no inbox; messages queue as before")
	}
}

// A message the turn never read runs as its own turn when the turn ends.
func TestSteer_LeftoverRunsAsOwnTurn_645(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &stopAwareReceiver{started: make(chan string, 4)}
	bot.SetReceiver(r)
	ctx, done := bot.beginTurn(100, false, 42)
	if !bot.offerSteer(context.Background(), &Message{ID: 9, ChatID: 100, UserID: 42, Text: "late message"}) {
		t.Fatal("offer refused")
	}
	_ = ctx
	done()
	if got := waitStarted(t, r); got != "late message" {
		t.Fatalf("leftover turn = %q", got)
	}
	_ = bot.handleStopCmd(context.Background(), 100, 42, []string{"/stop"})
	_ = dispatcher.ErrStoppedByUser
}

// Voice and empty messages, and commands, never steer (review cd48 F6, F8).
func TestSteer_VoiceEmptyAndCommandsDoNotSteer_645(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	_, done := bot.beginTurn(100, false, 42)
	defer done()
	if bot.offerSteer(context.Background(), &Message{ChatID: 100, UserID: 42, Text: "transcript", IsVoice: true}) {
		t.Fatal("a voice message must take the normal path")
	}
	if bot.offerSteer(context.Background(), &Message{ChatID: 100, UserID: 42, Text: "   "}) {
		t.Fatal("an empty message must not be offered")
	}
	// A command is answered by the command path and never reaches the inbox.
	_ = bot.HandleMessage(context.Background(), &Message{ID: 3, ChatID: 100, UserID: 42, Text: "/help"})
	bot.stops.mu.Lock()
	inbox := bot.stops.running[100].inbox
	bot.stops.mu.Unlock()
	if got := inbox.Drain(); len(got) != 0 {
		t.Fatalf("a command must not steer, inbox = %v", got)
	}
}

// After /stop, a steered message that was read is dropped with the turn; it
// does not come back as a new turn (review cd48 F2).
func TestSteer_StopDropsReadMessages_645(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &stopAwareReceiver{started: make(chan string, 4)}
	bot.SetReceiver(r)
	_, done := bot.beginTurn(100, false, 42)
	if !bot.offerSteer(context.Background(), &Message{ID: 5, ChatID: 100, UserID: 42, Text: "do Y instead"}) {
		t.Fatal("offer refused")
	}
	bot.stops.mu.Lock()
	inbox := bot.stops.running[100].inbox
	bot.stops.mu.Unlock()
	_ = inbox.Drain() // the turn read it
	_ = bot.handleStopCmd(context.Background(), 100, 42, []string{"/stop"})
	done()
	select {
	case s := <-r.started:
		t.Fatalf("a stopped turn's read message must not rerun, got %q", s)
	case <-time.After(150 * time.Millisecond):
	}
}

// A message offered just after the turn closed its inbox is refused and takes
// the normal path (review cd48 F4b, 20eb F8).
func TestSteer_OfferAfterCloseTakesNormalPath_645(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	_, done := bot.beginTurn(100, false, 42)
	bot.stops.mu.Lock()
	inbox := bot.stops.running[100].inbox
	bot.stops.mu.Unlock()
	_ = inbox.Close(false) // the turn closes between the registry read and the offer
	if bot.offerSteer(context.Background(), &Message{ChatID: 100, UserID: 42, Text: "late"}) {
		t.Fatal("an offer to a closed inbox must be refused so the message starts its own turn")
	}
	done()
}

// In a group chat, another member's message must not join a turn that runs
// with the starting member's project scope; it takes the normal path under
// its own scope (security review of be0fc3d80).
func TestSteer_OnlyTheTurnsSpeakerSteers_645(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	_, done := bot.beginTurn(-100, false, 42)
	defer done()
	if bot.offerSteer(context.Background(), &Message{ChatID: -100, UserID: 77, Text: "do something else"}) {
		t.Fatal("another member's message must not steer this turn")
	}
	if !bot.offerSteer(context.Background(), &Message{ChatID: -100, UserID: 42, Text: "my correction"}) {
		t.Fatal("the turn's own speaker steers it")
	}
}
