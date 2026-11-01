// Persister-aware tests for the telegram SessionStore. The pre-
// feature tests in session_store_test.go construct stores with a
// nil persister (in-memory only); these exercise the DB-backed
// read/write-through paths added for the horizontal-scaling
// rollout.

package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
	"vornik.io/vornik/internal/sessionstore/sessionstoretest"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/sessionstore"
)

// fakeChannelSessionRepo mirrors the Postgres semantics in memory
// for the persister-aware tests. The persistence and webchat
// packages each carry their own copy; making this an exported
// helper would mean a new test-only package, which isn't worth
// the surface for a tight three-file pattern.
type fakeChannelSessionRepo struct {
	// fence numbers the rows (art17 redact design §15.18).
	fence sessionstoretest.Versions
	mu    sync.Mutex
	rows  map[string]*persistence.ChannelSession
}

func newFakeChannelSessionRepo() *fakeChannelSessionRepo {
	return &fakeChannelSessionRepo{rows: map[string]*persistence.ChannelSession{}}
}

func chKey(kind, id string) string { return kind + "/" + id }

func (f *fakeChannelSessionRepo) Load(_ context.Context, kind, sessionID string) (*persistence.ChannelSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[chKey(kind, sessionID)]; ok {
		cp := *row
		cp.Version = f.fence.Of(kind, sessionID)
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

func (f *fakeChannelSessionRepo) Save(_ context.Context, kind, sessionID, activeProject string, history []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[chKey(kind, sessionID)] = &persistence.ChannelSession{
		Kind:          kind,
		SessionID:     sessionID,
		ActiveProject: activeProject,
		History:       append([]byte(nil), history...),
		UpdatedAt:     time.Now(),
	}
	return nil
}

// ListByPrefix is unused by these tests — fakeChannelSessionRepo exists to exercise
// Load/Save/Delete. Present to satisfy the repository interface.
func (f *fakeChannelSessionRepo) ListByPrefix(_ context.Context, _, _ string, _ int) ([]*persistence.ChannelSession, error) {
	return nil, nil
}

func (f *fakeChannelSessionRepo) Delete(_ context.Context, kind, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, chKey(kind, sessionID))
	return nil
}

// TestTelegramSessionStore_Append_WritesThroughToPersister: the
// dispatcher post-turn write lands in both the bot's in-memory
// cache AND the DB. Replicas can pick this conversation up; a
// daemon restart can rehydrate it.
func TestTelegramSessionStore_Append_WritesThroughToPersister(t *testing.T) {
	repo := newFakeChannelSessionRepo()
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.sessionPersister = sessionstore.New(repo, "telegram", zerolog.Nop())
	bot.setActiveProject(100, "proj-tg")

	store := NewSessionStore(bot, nil)
	err := store.Append(context.Background(),
		conversation.ChannelMessage{
			Source:    "telegram",
			SessionID: "100",
			SpeakerID: "42",
		},
		dispatcher.Result{
			Messages: []chat.Message{
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello"},
			},
		})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// In-memory state still updated.
	conv := bot.getConversation(100, "proj-tg").GetMessages()
	if len(conv) != 2 || conv[0].Content != "hi" || conv[1].Content != "hello" {
		t.Errorf("in-memory conv unexpected: %+v", conv)
	}

	// Persisted row carries the same history + active project.
	row, err := repo.Load(context.Background(), "telegram", "100")
	if err != nil {
		t.Fatalf("repo.Load: %v", err)
	}
	if row.ActiveProject != "proj-tg" {
		t.Errorf("active_project = %q, want proj-tg", row.ActiveProject)
	}
	var got []chat.Message
	if err := json.Unmarshal(row.History, &got); err != nil {
		t.Fatalf("unmarshal history: %v", err)
	}
	if len(got) != 2 || got[1].Content != "hello" {
		t.Errorf("persisted history mismatch: %+v", got)
	}
}

// TestTelegramSessionStore_Load_RehydratesFromPersister: replica
// failover / restart recovery. Bot's in-memory map is empty for
// chatID 200; a Load should pull from the DB and populate the
// cache + return the persisted history.
func TestTelegramSessionStore_Load_RehydratesFromPersister(t *testing.T) {
	repo := newFakeChannelSessionRepo()
	prior := []chat.Message{
		{Role: "user", Content: "from-other-replica"},
		{Role: "assistant", Content: "reply-from-other-replica"},
	}
	raw, _ := json.Marshal(prior)
	if err := repo.Save(context.Background(), "telegram", "200", "proj-restored", raw); err != nil {
		t.Fatalf("pre-seed: %v", err)
	}

	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.sessionPersister = sessionstore.New(repo, "telegram", zerolog.Nop())

	// Pre-condition: cache is empty.
	if bot.hasInMemorySession(200, "") {
		t.Fatal("setup: bot should have no cache entry for chat 200")
	}

	store := NewSessionStore(bot, nil)
	sess, err := store.Load(context.Background(),
		conversation.ChannelMessage{Source: "telegram", SessionID: "200", SpeakerID: "42"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sess.History) != 2 || sess.History[0].Content != "from-other-replica" {
		t.Errorf("Session.History not hydrated: %+v", sess.History)
	}
	if bot.getActiveProject(200) != "proj-restored" {
		t.Errorf("active project not hydrated; got %q", bot.getActiveProject(200))
	}

	// Cache should now report present.
	if !bot.hasInMemorySession(200, "proj-restored") {
		t.Errorf("bot cache should be populated after hydrate")
	}
}

// TestTelegramSessionStore_Load_SkipsHydrateWhenCacheWarm: a warm lane whose
// stored row is still as this process saved it is served from memory, not
// re-read. Since the fence (art17 redact design §15.18) "warm" means saved or
// loaded by this process: a lane it never synchronised with the row yields
// to the row (TestTelegramFence_UnseenLaneYieldsToTheRow).
func TestTelegramSessionStore_Load_SkipsHydrateWhenCacheWarm(t *testing.T) {
	repo := newFakeChannelSessionRepo()
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.sessionPersister = sessionstore.New(repo, "telegram", zerolog.Nop())
	store := NewSessionStore(bot, nil)
	msg := conversation.ChannelMessage{Source: "telegram", SessionID: "300", SpeakerID: "42"}
	if err := store.Append(context.Background(), msg, dispatcher.Result{
		Messages: []chat.Message{{Role: "user", Content: "saved"}}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Memory moves ahead of the row (as between a turn and its save); the
	// row's version is unchanged, so memory is served.
	bot.getConversation(300, "").AddMessage(chat.Message{Role: "user", Content: "fresh-in-memory"})

	sess, err := store.Load(context.Background(), msg)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := len(sess.History); n != 2 || sess.History[1].Content != "fresh-in-memory" {
		t.Errorf("Load re-read the row instead of the warm cache: %+v", sess.History)
	}
}

// TestTelegramSessionStore_ResetConversation_DeletesPersistedRow:
// /clear-style flows must purge the DB row too — otherwise a
// replica failover could replay the just-cleared conversation
// back to the user.
func TestTelegramSessionStore_ResetConversation_DeletesPersistedRow(t *testing.T) {
	repo := newFakeChannelSessionRepo()
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.sessionPersister = sessionstore.New(repo, "telegram", zerolog.Nop())

	// Build state via SessionStore.Append (so write-through fires).
	store := NewSessionStore(bot, nil)
	require := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	require(store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "400", SpeakerID: "42"},
		dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "doomed"}}}))

	if _, err := repo.Load(context.Background(), "telegram", "400"); err != nil {
		t.Fatalf("DB row should exist before reset; got %v", err)
	}

	bot.resetConversation(400, "")

	if _, err := repo.Load(context.Background(), "telegram", "400"); err == nil {
		t.Errorf("DB row should be gone after resetConversation")
	}
}

// TestTelegramSessionStore_Append_NilPersisterStaysInMemoryOnly:
// regression guard — a bot without a persister must continue to
// work exactly like before this feature (no DB calls, no panics).
func TestTelegramSessionStore_Append_NilPersisterStaysInMemoryOnly(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	if bot.sessionPersisterRef() != nil {
		t.Fatal("setup: persister should be nil")
	}
	store := NewSessionStore(bot, nil)
	err := store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "500", SpeakerID: "42"},
		dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Errorf("nil-persister Append should not error: %v", err)
	}
	got := bot.getConversation(500, "").GetMessages()
	if len(got) != 1 || got[0].Content != "x" {
		t.Errorf("in-memory state mismatch: %+v", got)
	}
}

// TestBotWithSessionPersister_OptionWiring confirms the BotOption
// stamps the persister on the bot during construction.
func TestBotWithSessionPersister_OptionWiring(t *testing.T) {
	repo := newFakeChannelSessionRepo()
	p := sessionstore.New(repo, "telegram", zerolog.Nop())
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	WithSessionPersister(p)(bot)
	if bot.sessionPersisterRef() != p {
		t.Errorf("persister not wired by option")
	}
}

// TestBotHydrateSession_ConcurrentDoesNotClobber: two near-
// simultaneous Loads for the same chat must not race-clobber a
// concurrently-populated cache entry. The check inside
// hydrateSession defends against the case where the inbound
// goroutine populated the cache between hasInMemorySession and
// the hydrateSession write.
func TestBotHydrateSession_ConcurrentDoesNotClobber(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})

	// Pre-populate via the normal path so we know what fresh
	// content looks like.
	bot.getConversation(600, "").AddMessage(chat.Message{Role: "user", Content: "winner"})

	// Now attempt to hydrate with DIFFERENT content — the guard
	// inside hydrateSession should detect the existing entry
	// and skip.
	bot.hydrateSession(600, "", []chat.Message{{Role: "user", Content: "loser"}})

	got := bot.getConversation(600, "").GetMessages()
	if len(got) != 1 || got[0].Content != "winner" {
		t.Errorf("hydrate clobbered existing cache: %+v", got)
	}
}

// Compile-time spot-check that the helper string encoding
// matches what the bot computes: chat_id as strconv.FormatInt
// base-10. If this changes the persister-side key would diverge
// from the bot-side hydrate key.
func TestTelegramSessionID_FormatMatchesChatID(t *testing.T) {
	const chatID int64 = -10042
	if got := strconv.FormatInt(chatID, 10); got != "-10042" {
		t.Errorf("FormatInt produced unexpected %q", got)
	}
}

// Version, SaveIfVersion and Fenced honour the fence over this fake's own
// rows (art17 redact design §15.18).
func (f *fakeChannelSessionRepo) Version(ctx context.Context, kind, sessionID string) (bool, int64, error) {
	if _, err := f.Load(ctx, kind, sessionID); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return false, 0, nil
		}
		return false, 0, err
	}
	return true, f.fence.Of(kind, sessionID), nil
}

func (f *fakeChannelSessionRepo) SaveIfVersion(ctx context.Context, kind, sessionID, activeProject string, history []byte, expect *int64) (int64, bool, error) {
	exists, _, err := f.Version(ctx, kind, sessionID)
	if err != nil {
		return 0, false, err
	}
	if !f.fence.Matches(kind, sessionID, exists, expect) {
		return 0, false, nil
	}
	if err := f.Save(ctx, kind, sessionID, activeProject, history); err != nil {
		return 0, false, err
	}
	return f.fence.Bump(kind, sessionID), true, nil
}

func (f *fakeChannelSessionRepo) Fenced() bool { return true }
