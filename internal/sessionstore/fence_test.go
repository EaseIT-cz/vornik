package sessionstore

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
)

// The fence (GitHub #344 slice 5d-2, art17 redact design §15.18). Found at:
// review, observed at integration (§15.10.1): a session an erasure deleted
// was written back by the daemon's next save, and its in-memory copy kept
// feeding the model.

func msgs(text string) []chat.Message { return []chat.Message{{Role: "user", Content: text}} }

func TestFence_ErasedRowIsNotFreshAndIsNotWrittenBack(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepo()
	p := New(repo, "slack", zerolog.Nop())
	if err := p.Save(ctx, "s", "p", msgs("hello")); err != nil {
		t.Fatal(err)
	}
	if !p.Fresh(ctx, "s") {
		t.Fatal("our own write must leave the session fresh")
	}
	repo.eraseBehind("slack", "s")
	if p.Fresh(ctx, "s") {
		t.Fatal("a deleted row must not read as fresh")
	}
	// A turn that loaded before the erasure saves after it: refused.
	if err := p.Save(ctx, "s", "p", msgs("hello, again")); !errors.Is(err, ErrSessionMoved) {
		t.Fatalf("save over an erased row = %v, want ErrSessionMoved", err)
	}
	if _, err := repo.Load(ctx, "slack", "s"); err == nil {
		t.Fatal("the erased row was written back")
	}
	// After the refusal the store reloads; a new conversation may start.
	if _, _, found, _ := p.Load(ctx, "s"); found {
		t.Fatal("erased row reappeared")
	}
	if err := p.Save(ctx, "s", "p", msgs("new")); err != nil {
		t.Fatalf("a new conversation after the reload: %v", err)
	}
}

func TestFence_AnotherWriterMovesTheSession(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepo()
	a, b := New(repo, "email", zerolog.Nop()), New(repo, "email", zerolog.Nop())
	if err := a.Save(ctx, "t", "", msgs("1")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := b.Load(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if err := b.Save(ctx, "t", "", msgs("2")); err != nil {
		t.Fatal(err)
	}
	if a.Fresh(ctx, "t") {
		t.Fatal("a's copy is stale after b's write")
	}
	if err := a.Save(ctx, "t", "", msgs("lost")); !errors.Is(err, ErrSessionMoved) {
		t.Fatalf("a's stale save = %v", err)
	}
}

func TestFence_ReadErrorFailsClosedAndCounts(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepo()
	p := New(repo, "webchat", zerolog.Nop())
	var errs int
	p.OnFenceError = func() { errs++ }
	if err := p.Save(ctx, "w", "", msgs("x")); err != nil {
		t.Fatal(err)
	}
	repo.versionErr = errors.New("db down")
	if p.Fresh(ctx, "w") {
		t.Fatal("a fence read error must not serve memory")
	}
	if errs != 1 {
		t.Fatalf("fence errors counted %d", errs)
	}
}

func TestFence_UnknownSessionIsNotFresh(t *testing.T) {
	// Memory the persister never loaded or saved (a lane restored from a
	// file) has nothing to compare: not fresh, so the store reloads.
	if New(newFakeRepo(), "telegram", zerolog.Nop()).Fresh(context.Background(), "42") {
		t.Fatal("unknown session read as fresh")
	}
}

func TestFence_UnfencedOrNilRepoIsAlwaysFresh(t *testing.T) {
	if !New(nil, "slack", zerolog.Nop()).Fresh(context.Background(), "s") {
		t.Fatal("nil repo")
	}
	var p *Persister
	if !p.Fresh(context.Background(), "s") {
		t.Fatal("nil persister")
	}
}

func TestFence_ClearChatThenNewMessage(t *testing.T) {
	ctx := context.Background()
	p := New(newFakeRepo(), "webchat", zerolog.Nop())
	_ = p.Save(ctx, "w", "", msgs("a"))
	if err := p.Delete(ctx, "w"); err != nil {
		t.Fatal(err)
	}
	if !p.Fresh(ctx, "w") {
		t.Fatal("our own delete leaves the (absent) session fresh")
	}
	if err := p.Save(ctx, "w", "", msgs("b")); err != nil {
		t.Fatalf("save after our own clear: %v", err)
	}
}
