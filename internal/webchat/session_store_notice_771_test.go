package webchat

// GitHub #771 (web-chat-design.md §5 N3): AppendNotice stores a task
// completion notice in a web chat history: through the persister, onto a
// cold store's persisted history, once per text. Found at: production
// (operator, 2026-11-14). Regression test at: integration (the store over
// the session persister).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/sessionstore"
)

func TestSessionStore771_AppendNoticeOnAColdStore(t *testing.T) {
	repo := newFakeChannelSessionRepo()
	prior := []chat.Message{{Role: "user", Content: "schedule it"}, {Role: "assistant", Content: "Scheduled."}}
	raw, _ := json.Marshal(prior)
	require.NoError(t, repo.Save(context.Background(), "webchat", "operator:c", "proj", raw))

	s := NewSessionStore(nil, "proj")
	s.SetPersister(sessionstore.New(repo, "webchat", zerolog.Nop()))
	require.NoError(t, s.AppendNotice(context.Background(), "operator:c", "✅ done"))
	require.NoError(t, s.AppendNotice(context.Background(), "operator:c", "✅ done"))

	hist := s.History("operator:c")
	require.Len(t, hist, 3, "the persisted history, then the notice once")
	assert.Equal(t, "Scheduled.", hist[1].Content)
	assert.Equal(t, NoticeName, hist[2].Name)
	assert.Equal(t, "assistant", hist[2].Role)

	saved, err := repo.Load(context.Background(), "webchat", "operator:c")
	require.NoError(t, err)
	var stored []chat.Message
	require.NoError(t, json.Unmarshal(saved.History, &stored))
	require.Len(t, stored, 3, "the notice is written through")
	assert.Equal(t, NoticeName, stored[2].Name)
}

func TestSessionStore771_AppendNoticeNeedsAConversation(t *testing.T) {
	s := NewSessionStore(nil, "proj")
	assert.True(t, errors.Is(s.AppendNotice(context.Background(), "operator:none", "x"), ErrUnknownSession))
}
