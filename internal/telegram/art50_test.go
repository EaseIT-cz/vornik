package telegram

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: GitHub #342 rows 13 and 14 — the task-completion notify and
// the artifact uploads called the Bot API directly, so a chat that never had a
// ChannelReceiver turn received AI-authored results and files with no Art 50
// notice. Telegram is per-session: the notice must arrive first, as its own
// message, once per chat (design §11.4).

const noticeMarker = "You are interacting with an AI system"

type servedAll struct{}

func (servedAll) WasServed(context.Context, string, string) (bool, error)  { return true, nil }
func (servedAll) MarkServed(context.Context, string, string, string) error { return nil }

// alreadyDisclosed is the disclosure service for test bots whose chats were
// told in an earlier turn — what every pre-#342 test implicitly assumed. The
// Art 50 tests below opt out of it explicitly.
func alreadyDisclosed() *aidisclosure.Service {
	return aidisclosure.New(aidisclosure.Config{}, servedAll{})
}

type servedMem map[string]bool

func (m servedMem) WasServed(_ context.Context, ch, s string) (bool, error) { return m[ch+"|"+s], nil }
func (m servedMem) MarkServed(_ context.Context, ch, s, _ string) error {
	m[ch+"|"+s] = true
	return nil
}

func isNotice(c apiCall) bool {
	return strings.HasSuffix(c.Path, "/sendMessage") && strings.Contains(c.Body, noticeMarker)
}

func TestArt50_NotifyTaskCompletedDisclosesFirstOncePerChat(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.SetAIDisclosure(aidisclosure.New(aidisclosure.Config{}, servedMem{}))
	bot.watcherRepo = &stubWatcherRepo{getFn: func(context.Context, string) ([]int64, error) { return []int64{111}, nil }}

	for i := 0; i < 2; i++ {
		bot.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_a", ProjectID: "p"}, true, "the result")
	}
	if len(*calls) != 3 {
		t.Fatalf("want notice + 2 notifications, got %d calls: %+v", len(*calls), *calls)
	}
	if !isNotice((*calls)[0]) {
		t.Fatalf("first message must be the AI notice, got %q", (*calls)[0].Body)
	}
	for _, c := range (*calls)[1:] {
		if isNotice(c) {
			t.Fatal("the notice was re-sent to an already-disclosed chat")
		}
	}
}

func TestArt50_ArtifactUploadDisclosesFirst(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.SetAIDisclosure(nil) // never disclosed to: nil = compiled defaults, no memory
	if err := bot.SendDocumentReader(context.Background(), 222, strings.NewReader("pdf"), "r.pdf", "Scheduled report"); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || !isNotice((*calls)[0]) || !strings.HasSuffix((*calls)[1].Path, "/sendDocument") {
		t.Fatalf("want notice then document, got %+v", *calls)
	}
}

func TestArt50_SummaryDisclosesBeforeLLMOutput(t *testing.T) {
	prov := &stubChatProvider{completeFn: func(context.Context, []chat.Message) (*chat.ChatResponse, error) {
		resp := chat.ChatResponse{}
		resp.Choices = append(resp.Choices, struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{Message: chat.Message{Role: "assistant", Content: "SUMMARY"}})
		return &resp, nil
	}}
	bot, calls, cleanup := makeSummarizeBot(t, prov)
	defer cleanup()
	bot.SetAIDisclosure(nil)
	conv := bot.getConversation(333, "")
	conv.AddMessage(chat.Message{Role: "user", Content: "hi"})
	if err := bot.handleSummarize(context.Background(), 333); err != nil {
		t.Fatal(err)
	}
	noticeAt, summaryAt := -1, -1
	for i, c := range *calls {
		if isNotice(c) && noticeAt < 0 {
			noticeAt = i
		}
		if strings.Contains(c.Body, "SUMMARY") {
			summaryAt = i
		}
	}
	if noticeAt < 0 || summaryAt < 0 || noticeAt > summaryAt {
		t.Fatalf("notice at %d, summary at %d: the LLM summary must follow the AI notice", noticeAt, summaryAt)
	}
}
