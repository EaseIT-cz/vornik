package dispatcher

// GitHub #642 (conversation-channel design, update 2026-11-05). Found at:
// production, 2026-11-04: one Telegram turn showed about 37 status lines
// above the answer, because every tool call wrote a marker paragraph into the
// streamed text.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

type recordingTurnStatus struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingTurnStatus) ReportTurnStatus(_ context.Context, _, _, status string) {
	r.mu.Lock()
	r.lines = append(r.lines, status)
	r.mu.Unlock()
}

// toolSequenceProvider asks for the named tools one call at a time, then
// answers.
type toolSequenceProvider struct {
	ctxCapturingProvider
	tools []string
	n     int
}

func (p *toolSequenceProvider) next() *chat.ChatResponse {
	r := &chat.ChatResponse{Model: "m"}
	msg := chat.Message{Role: "assistant", Content: "the answer"}
	if p.n < len(p.tools) {
		msg = chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "c" + strings.Repeat("x", p.n+1), Type: "function",
			Function: chat.FunctionCall{Name: p.tools[p.n], Arguments: "{}"}}}}
	}
	p.n++
	r.Choices = append(r.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: msg})
	return r
}
func (p *toolSequenceProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	return p.next(), nil
}
func (p *toolSequenceProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, onText chat.StreamCallback) (*chat.ChatResponse, error) {
	r := p.next()
	if onText != nil && r.Choices[0].Message.Content != "" {
		onText(r.Choices[0].Message.Content)
	}
	return r, nil
}

func TestStreamedTurnCarriesNoToolMarkers_642(t *testing.T) {
	p := &toolSequenceProvider{tools: []string{"alpha_tool", "beta_tool", "alpha_tool"}}
	rec := &recordingTurnStatus{}
	a := NewAgent(p, nil, nil, nil, nil, WithMaxIterations(10))
	a.SetTurnStatusReporter(rec)
	var streamed string
	res := a.ProcessStreaming(context.Background(), Request{
		Messages: []chat.Message{{Role: "user", Content: "go"}}, Project: "assistant",
		OriginatingChannel: "telegram", OriginatingSessionID: "555",
	}, func(acc string) { streamed = acc })
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if strings.Contains(streamed, "[") || strings.Contains(streamed, "⏳") {
		t.Fatalf("the streamed text must carry only the model's text, got %q", streamed)
	}
	if streamed != "the answer" {
		t.Fatalf("streamed = %q, want the answer alone", streamed)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.lines) != 3 {
		t.Fatalf("one status per tool call, got %d: %q", len(rec.lines), rec.lines)
	}
	last := rec.lines[2]
	lt := strings.TrimSuffix(toolStatusLine("alpha_tool"), "…")
	ms := strings.TrimSuffix(toolStatusLine("beta_tool"), "…")
	if !strings.Contains(last, lt+" ×2") || !strings.Contains(last, ms) || strings.Index(last, lt) > strings.Index(last, ms) {
		t.Fatalf("summary must count per phrase in first-seen order: %q", last)
	}
	if !strings.HasPrefix(last, "⏳ "+lt+" · ") {
		t.Fatalf("summary must lead with the current activity: %q", last)
	}
}

func TestTurnProgressRendering_642(t *testing.T) {
	start := time.Unix(0, 0)
	p := newTurnProgress(start)
	p.noteAt("list_tasks", start)
	p.noteAt("memory_search", start)
	got := p.noteAt("list_tasks", start.Add(42*time.Second))
	want := "⏳ checking your tasks · 42s · so far: checking your tasks ×2, searching memory"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// MCP tools keep a readable name (review c257 F4).
func TestToolStatusLineMCP_642(t *testing.T) {
	if got := toolStatusLine("mcp__scraper__web_fetch"); got != "using scraper/web_fetch…" {
		t.Fatalf("got %q", got)
	}
}
