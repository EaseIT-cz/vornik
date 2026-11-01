package telegram

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/sessionstore"
)

// The fence in the Telegram bot (GitHub #344 slice 5d-2, art17 redact design
// §15.18 and §15.18.1). Found at: review, observed at integration
// (§15.10.1): an erased chat's lanes kept feeding the model and commands, and
// the bot's file snapshot kept a copy outside the database.

func fencedBot(t *testing.T, cfg BotConfig) (*Bot, *fakeChannelSessionRepo, *telegramRecorder) {
	t.Helper()
	if cfg.AllowedUsers == nil {
		cfg.AllowedUsers = map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}}
	}
	cfg.Token = "tok"
	repo := newFakeChannelSessionRepo()
	b, rec := newBotWithRecorder(t, cfg, WithSessionPersister(sessionstore.New(repo, "telegram", zerolog.Nop())))
	return b, repo, rec
}

func saveTurn(t *testing.T, b *Bot, chatID string, texts ...string) {
	t.Helper()
	res := dispatcher.Result{}
	for _, x := range texts {
		res.Messages = append(res.Messages, chat.Message{Role: "user", Content: x})
	}
	if err := NewSessionStore(b, nil).Append(context.Background(),
		conversation.ChannelMessage{SessionID: chatID, SpeakerID: "42"}, res); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramFence_ACommandDoesNotReadAnErasedChat(t *testing.T) {
	b, repo, rec := fencedBot(t, BotConfig{})
	saveTurn(t, b, "100", "my address is 1 Main St")
	_ = repo.Delete(context.Background(), "telegram", "100") // the erasure
	// A second, never-persisted lane of the same chat must go too.
	b.getConversation(100, "other-project").AddMessage(chat.Message{Role: "user", Content: "side lane"})

	runCommand(t, b, rec, 100, 42, "/context")
	if n := b.getConversation(100, "").Len(); n != 0 {
		t.Fatalf("/context ran on an erased chat's lane (%d messages)", n)
	}
	if n := b.getConversation(100, "other-project").Len(); n != 0 {
		t.Fatalf("a side lane of the erased chat survived (%d messages)", n)
	}
}

func TestTelegramFence_ATurnInFlightDoesNotWriteAnErasedChatBack(t *testing.T) {
	b, repo, _ := fencedBot(t, BotConfig{})
	store := NewSessionStore(b, nil)
	msg := conversation.ChannelMessage{SessionID: "200", SpeakerID: "42"}
	saveTurn(t, b, "200", "first")
	if _, err := store.Load(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	_ = repo.Delete(context.Background(), "telegram", "200")
	saveTurn(t, b, "200", "first", "second")
	if _, err := repo.Load(context.Background(), "telegram", "200"); err == nil {
		t.Fatal("the turn in flight wrote the erased chat back")
	}
	if b.hasAnyInMemorySession(200) {
		t.Fatal("the refused turn is still held in memory")
	}
}

func TestTelegramFence_SnapshotIsNeitherLoadedNorWrittenWithAPersister(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := chat.SaveConversations(path, map[int64]*chat.Conversation{300: func() *chat.Conversation {
		c := chat.NewConversation("x", 10)
		c.AddMessage(chat.Message{Role: "user", Content: "old snapshot"})
		return c
	}()}); err != nil {
		t.Fatal(err)
	}
	b, _, _ := fencedBot(t, BotConfig{SessionPath: path})
	b.restoreConversations()
	if b.hasAnyInMemorySession(300) {
		t.Fatal("the superseded snapshot was loaded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the superseded snapshot was not removed: %v", err)
	}
	saveTurn(t, b, "300", "new")
	b.saveConversations()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a snapshot was written although the database holds the sessions")
	}
}
