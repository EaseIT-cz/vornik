package ui

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/webchat"
)

// Task completion notices in the web chat (web-chat-design.md §5, GitHub
// #771). The completion notifier hands a notice for a task the chat started
// to DeliverWebChatNotice; it is stored in the history it came from, and an
// open page reads it from ChatNotices.

// chatNotice is a notice waiting for a running turn to end (§5 N4).
type chatNotice struct{ projectID, text string }

// chatNoticeWrite bounds one notice's store write.
const chatNoticeWrite = 5 * time.Second

// DeliverWebChatNotice stores a completion notice in historyKey's chat (§5
// N3, N4). The store is the project store that holds the key, else the
// project the session persister recorded for it; with neither it is
// webchat.ErrUnknownSession. While the key has a running turn the notice
// waits for that turn's end, because the turn's own write replaces the
// history.
func (s *Server) DeliverWebChatNotice(ctx context.Context, historyKey, text string) error {
	projectID := s.noticeProject(ctx, historyKey)
	if projectID == "" {
		return webchat.ErrUnknownSession
	}
	return s.chatTurnRegistry().deliverNotice(historyKey, chatNotice{projectID: projectID, text: text}, func(n chatNotice) error {
		return s.chatStoreFor(n.projectID).AppendNotice(ctx, historyKey, n.text)
	})
}

// deliverNotice queues n on historyKey's running turn, or stores it now with
// write; both under the registry lock, so a turn that starts after the write
// loads the history with the notice in it.
func (g *chatTurnRegistry) deliverNotice(historyKey string, n chatNotice, write func(chatNotice) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t := g.running[historyKey]; t != nil {
		t.notices = append(t.notices, n)
		return nil
	}
	return write(n)
}

// storeQueuedNotice stores a notice a turn queued, when the turn ends.
func (s *Server) storeQueuedNotice(historyKey string, n chatNotice) {
	ctx, cancel := context.WithTimeout(context.Background(), chatNoticeWrite)
	defer cancel()
	if err := s.chatStoreFor(n.projectID).AppendNotice(ctx, historyKey, n.text); err != nil {
		s.logger.Warn().Err(err).Str("project", n.projectID).Msg("web chat: a task notice could not be stored after its turn")
	}
}

// noticeProject is the project whose chat store holds historyKey: one that
// has it cached, else the one the persister recorded.
func (s *Server) noticeProject(ctx context.Context, historyKey string) string {
	s.chatStoresMu.Lock()
	for id, st := range s.chatStores {
		if st.Has(historyKey) {
			s.chatStoresMu.Unlock()
			return id
		}
	}
	s.chatStoresMu.Unlock()
	if s.chatSessionPersister == nil {
		return ""
	}
	if _, project, found, err := s.chatSessionPersister.Load(ctx, historyKey); err == nil && found {
		return project
	}
	return ""
}

// chatNotices is every notice in a history, in order.
func chatNotices(history []chat.Message) []chat.Message {
	var out []chat.Message
	for _, m := range history {
		if m.Name == webchat.NoticeName {
			out = append(out, m)
		}
	}
	return out
}

// ChatNotices handles GET /ui/projects/<id>/chat/notices?after=<n> (§5 N6):
// {"count": c}, and "html" with the notices after the first n when there
// are more. The key is the requester's own cookie and view, so another
// browser, view or project reads its own history, never this one. What it
// returns is marked (Art 50(2), §5 N7).
func (s *Server) ChatNotices(w http.ResponseWriter, r *http.Request, projectID string) {
	if api.ErrorTextAccessFor(r, s.operatorCapability.IsAdminKey) == api.ErrorTextDenied {
		api.RespondProjectScopeRequired(w)
		return
	}
	// A missing or malformed count reads as 0: the page then gets every
	// notice again and sets its count from the answer (code review 2756 F4).
	after, err := strconv.Atoi(r.URL.Query().Get("after"))
	if err != nil || after < 0 {
		after = 0
	}
	key := s.chatRequestKey(r)
	var notices []chat.Message
	if key != "" {
		notices = chatNotices(s.chatStoreFor(projectID).History(key))
	}
	body := map[string]any{"count": len(notices)}
	if len(notices) > after {
		fresh := notices[after:]
		texts := make([]string, 0, len(fresh))
		for _, m := range fresh {
			texts = append(texts, m.Content)
		}
		marks := s.markChatReplies(r.Context(), w.Header(), texts, fresh)
		body["html"] = s.renderChatExchanges(buildChatExchanges(fresh, marks), projectID)
	}
	writeChatJSON(w, http.StatusOK, body)
}
