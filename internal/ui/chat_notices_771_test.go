package ui

// GitHub #771 (web-chat-design.md §5): a task the chat started reports its
// completion into the chat. The notice is stored in the browser's history
// for the view, survives a turn that is running when it arrives, renders as
// its own entry, and reaches an open page through the notices poll. Found
// at: production (operator, 2026-11-14: "I'll report back when it lands",
// and nothing came). Regression test at: integration (the real handlers,
// registry and store).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/webchat"
)

const notice771 = "✅ **Finished** the job you asked for: task `task_1`.\n\n[Open the task](/ui/tasks/task_1)"

func seedHistory771(t *testing.T, s *Server, key string) {
	t.Helper()
	require.NoError(t, s.chatStoreFor("p1").Append(context.Background(), conversation.ChannelMessage{SessionID: key},
		dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "schedule it"}, {Role: "assistant", Content: "Scheduled task_1."}}}))
}

func notices771(t *testing.T, s *Server, cookie, project, after string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ChatNotices(rec, turnRequest(http.MethodGet, "/projects/"+project+"/chat/notices?after="+after, cookie, nil), project)
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func TestChatNotice771_IsStoredAndRendersAsItsOwnEntry(t *testing.T) {
	s := turnServer(t, newParked("x"))
	key := chatHistoryKey("c1", true)
	seedHistory771(t, s, key)
	require.NoError(t, s.DeliverWebChatNotice(context.Background(), key, notice771))
	require.NoError(t, s.DeliverWebChatNotice(context.Background(), key, notice771)) // a restart's duplicate
	hist := s.chatStoreFor("p1").History(key)
	n := 0
	for _, m := range hist {
		if m.Name == webchat.NoticeName {
			n++
		}
	}
	assert.Equal(t, 1, n, "the same notice twice is stored once")
	ex := buildChatExchanges(hist, nil)
	require.Len(t, ex, 2)
	assert.Equal(t, "Scheduled task_1.", ex[0].Reply, "the notice must not take the earlier exchange's reply")
	assert.True(t, ex[1].Notice)
	page := httptest.NewRecorder()
	s.ChatPage(page, turnRequest(http.MethodGet, "/projects/p1/chat", "c1", nil), "p1")
	body := page.Body.String()
	assert.Contains(t, body, "Task update")
	assert.Contains(t, body, `href="/ui/tasks/task_1"`)
	assert.Contains(t, body, `data-notices="1"`)
}

func TestChatNotice771_UnknownHistoryIsDropped(t *testing.T) {
	s := turnServer(t, newParked("x"))
	assert.Error(t, s.DeliverWebChatNotice(context.Background(), chatHistoryKey("nobody", true), notice771))
}

func TestChatNotice771_SurvivesARunningTurn(t *testing.T) {
	for _, end := range []string{"done", "cancelled", "failed"} {
		t.Run(end, func(t *testing.T) {
			d := newParked("the reply")
			s := turnServer(t, d)
			if end == "failed" {
				s.chatTurnTimeout = 200 * time.Millisecond
			}
			key := chatHistoryKey("c1", true)
			seedHistory771(t, s, key)
			_, turn := startTurn(t, s, "c1", "another question")
			d.waitStarted(t)
			require.NoError(t, s.DeliverWebChatNotice(context.Background(), key, notice771))
			switch end {
			case "done":
				close(d.release)
			case "failed": // the turn's time limit ends it
			default:
				rec := httptest.NewRecorder()
				s.ChatTurnCancel(rec, turnRequest(http.MethodPost, "/projects/p1/chat/turns/"+turn+"/cancel", "c1", nil), "p1", turn)
			}
			pollUntil(t, s, turn, end)
			hist := s.chatStoreFor("p1").History(key)
			require.NotEmpty(t, hist)
			last := hist[len(hist)-1]
			assert.Equal(t, webchat.NoticeName, last.Name, "the notice queued during the turn is stored after it")
			assert.Equal(t, notice771, last.Content)
		})
	}
}

func TestChatNotice771_NoticesPoll(t *testing.T) {
	s := turnServer(t, newParked("x"))
	marker, _, _ := uiTestMarker()
	s.aiMarker = marker
	key := chatHistoryKey("c1", true)
	seedHistory771(t, s, key)
	require.NoError(t, s.DeliverWebChatNotice(context.Background(), key, notice771))

	rec := httptest.NewRecorder()
	s.ChatNotices(rec, turnRequest(http.MethodGet, "/projects/p1/chat/notices?after=0", "c1", nil), "p1")
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.EqualValues(t, 1, out["count"])
	assert.Contains(t, out["html"], "Open the task")
	// Art 50(2) (§5 N7): the notice is marked, in the HTML and the headers.
	html, _ := out["html"].(string)
	assert.Len(t, dataMarkRE.FindAllStringSubmatch(html, -1), 1)
	assert.Equal(t, "ai-generated", rec.Header().Get("AI-Disclosure"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	out = notices771(t, s, "c1", "p1", "1")
	assert.EqualValues(t, 1, out["count"])
	assert.Nil(t, out["html"])
	out = notices771(t, s, "c1", "p1", "garbage")
	assert.Contains(t, out["html"], "Open the task", "a malformed count reads as 0")
	for name, c := range map[string][2]string{"another cookie": {"c2", "p1"}, "another project": {"c1", "p2"}} {
		out = notices771(t, s, c[0], c[1], "0")
		assert.EqualValues(t, 0, out["count"], name)
		assert.Nil(t, out["html"], name)
	}
	// The member view of the same cookie is another history.
	rec = httptest.NewRecorder()
	req := scopedUIRequest(http.MethodGet, "/projects/p1/chat/notices?after=0", []string{"p1"})
	req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: "c1"})
	s.ChatNotices(rec, req, "p1")
	assert.NotContains(t, rec.Body.String(), "Open the task", "the member view sees the operator view's notice")
	// After New chat the count drops and nothing is sent.
	s.chatStoreFor("p1").Reset(key)
	out = notices771(t, s, "c1", "p1", "1")
	assert.EqualValues(t, 0, out["count"])
	assert.Nil(t, out["html"])
}

func TestChatNotice771_ChannelSaysItCanDeliverLater(t *testing.T) {
	ch := webchat.New("p1", conversation.Speaker{})
	var l conversation.LaterDeliveryChannel = ch
	assert.False(t, l.CanDeliverLater(), "false unless delivery is wired")
	assert.True(t, webchat.New("p1", conversation.Speaker{}, webchat.WithLaterDelivery(true)).CanDeliverLater())
}

// The #491 rule: an unscoped non-admin caller is refused the notices, as
// every chat endpoint refuses it.
func TestChatNotice771_UnscopedNonAdminIs403(t *testing.T) {
	c := newUIChain466With(t, true, &countingExplainer491{})
	code, body := c.serve(t, http.MethodGet, "/ui/projects/alpha/chat/notices?after=0", staticKey491UI)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, "PROJECT_SCOPE_REQUIRED")
}
