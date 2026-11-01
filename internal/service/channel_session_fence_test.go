package service

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/sessionstore"
)

// The channel-session fence in the Slack, email and GitHub stores (GitHub
// #344 slice 5d-2, art17 redact design §15.18). Found at: review, observed at
// integration (§15.10.1): a stored session deleted by an erasure was served
// from the store's memory on the next turn and written back, history and all,
// by its Append.

type fencedStore interface {
	dispatcher.SessionStore
	snapshotHistory(sessionID string) []chat.Message
}

func fenceStores(repo *fakeChanSessionRepo) map[string]fencedStore {
	sl := newSlackSessionStore(nil, "p")
	sl.SetPersister(sessionstore.New(repo, "slack", zerolog.Nop()))
	em := newEmailSessionStore(nil, "p", nil)
	em.SetPersister(sessionstore.New(repo, "email", zerolog.Nop()))
	gh := newGitHubSessionStore(nil, "p")
	gh.SetPersister(sessionstore.New(repo, "github", zerolog.Nop()))
	return map[string]fencedStore{"slack": sl, "email": em, "github": gh}
}

func turn(texts ...string) dispatcher.Result {
	out := dispatcher.Result{}
	for _, t := range texts {
		out.Messages = append(out.Messages, chat.Message{Role: "user", Content: t})
	}
	return out
}

func TestChannelFence_AnErasedSessionIsNotServedFromMemory(t *testing.T) {
	ctx := context.Background()
	repo := newFakeChanSessionRepo()
	for kind, store := range fenceStores(repo) {
		msg := conversation.ChannelMessage{SessionID: "T1/C1#100.1"}
		if err := store.Append(ctx, msg, turn("my phone is 555 0100")); err != nil {
			t.Fatal(err)
		}
		_ = repo.Delete(ctx, kind, msg.SessionID) // the erasure, from vornikctl
		sess, err := store.Load(ctx, msg)
		if err != nil {
			t.Fatal(err)
		}
		if len(sess.History) != 0 {
			t.Errorf("%s: an erased session was served from memory: %+v", kind, sess.History)
		}
		if got := store.snapshotHistory(msg.SessionID); len(got) != 0 {
			t.Errorf("%s: the erased session is still held: %+v", kind, got)
		}
	}
}

func TestChannelFence_ATurnInFlightDoesNotWriteAnErasedSessionBack(t *testing.T) {
	ctx := context.Background()
	repo := newFakeChanSessionRepo()
	for kind, store := range fenceStores(repo) {
		msg := conversation.ChannelMessage{SessionID: "T1/C1#200.1"}
		if err := store.Append(ctx, msg, turn("first")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(ctx, msg); err != nil { // the turn starts
			t.Fatal(err)
		}
		_ = repo.Delete(ctx, kind, msg.SessionID) // the erasure lands mid-turn
		if err := store.Append(ctx, msg, turn("first", "second")); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Load(ctx, kind, msg.SessionID); err == nil {
			t.Errorf("%s: the turn in flight wrote the erased session back", kind)
		}
		if got := store.snapshotHistory(msg.SessionID); len(got) != 0 {
			t.Errorf("%s: the refused turn is still held in memory: %+v", kind, got)
		}
	}
}

func TestSlackFence_ReadThreadAndDigestDoNotServeAnErasedThread(t *testing.T) {
	ctx := context.Background()
	repo := newFakeChanSessionRepo()
	store := fenceStores(repo)["slack"].(*slackSessionStore)
	thread := conversation.ChannelMessage{SessionID: "T1/C1#300.1"}
	if err := store.Append(ctx, thread, turn("thread text")); err != nil {
		t.Fatal(err)
	}
	_ = repo.Delete(ctx, "slack", thread.SessionID)
	if got, _ := store.ReadThread(ctx, thread.SessionID); len(got) != 0 {
		t.Fatalf("ReadThread served an erased thread from memory: %+v", got)
	}
	// The durable listing is now empty; the digest must not fall back to
	// memory, which could still hold the thread on another code path.
	store.mu.Lock()
	store.history[thread.SessionID] = []chat.Message{{Role: "user", Content: "held"}}
	store.mu.Unlock()
	if block := store.threadDigestBlock(ctx, "T1/C1#"+"main"); block != "" {
		t.Fatalf("the digest fell back to memory with persistence wired:\n%s", block)
	}
}
