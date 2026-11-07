package dispatcher

// GitHub #646 (dispatcher turn context design, section 6). Found at:
// production, 2026-11-05: dispatcher calls averaged 207 KB of prompt, because
// every tool result of a turn was resent in full on every later call.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"vornik.io/vornik/internal/chat"
)

func batch(id string, tool string, result string) []chat.Message {
	return []chat.Message{
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: id, Type: "function", Function: chat.FunctionCall{Name: tool, Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: id, Name: tool, Content: result},
	}
}

func isCompacted(m chat.Message) bool {
	return strings.HasPrefix(m.Content, "[earlier tool result shortened: ")
}

var big = strings.Repeat("a", 20000)

// Earlier turns are compacted; in the current turn the newest two batches stay
// whole and an older large one is compacted.
func TestCompactTurnToolResults_646(t *testing.T) {
	msgs := []chat.Message{{Role: "user", Content: "earlier turn"}}
	msgs = append(msgs, batch("h1", "web_fetch", big)...)
	msgs = append(msgs, batch("h2", "memory_search", big)...)
	msgs = append(msgs, chat.Message{Role: "assistant", Content: "earlier answer"}, chat.Message{Role: "user", Content: "now"})
	msgs = append(msgs, batch("c1", "web_fetch", big)...)
	msgs = append(msgs, batch("c2", "get_task_status", "small")...)
	msgs = append(msgs, batch("c3", "memory_search", big)...)
	msgs = append(msgs, batch("c4", "web_fetch", big)...)
	msgs[2].CachePrefix = true
	orig := append([]chat.Message(nil), msgs...)

	out, stat := compactTurnToolResults(msgs, 6)

	if len(out) != len(msgs) {
		t.Fatalf("message count changed: %d -> %d", len(msgs), len(out))
	}
	for i := range msgs {
		if !reflect.DeepEqual(msgs[i], orig[i]) {
			t.Fatalf("the input slice was mutated at %d", i)
		}
		// Every field but a compacted Content is unchanged (review 3b30 New-7).
		got := out[i]
		got.Content = msgs[i].Content
		if !reflect.DeepEqual(got, msgs[i]) {
			t.Fatalf("message %d changed beyond Content: %+v", i, out[i])
		}
	}
	want := fmt.Sprintf("[earlier tool result shortened: web_fetch returned 20000 characters; showing the first %d. Call the tool again if you need the rest.]", compactHeadRunes)
	if !strings.HasPrefix(out[2].Content, want) || len(out[2].Content) > compactHeadRunes+400 {
		t.Fatalf("earlier-turn result not compacted with the marker: %q", out[2].Content[:min(len(out[2].Content), 160)])
	}
	if !isCompacted(out[4]) {
		t.Fatal("every large earlier-turn result is compacted")
	}
	if !isCompacted(out[8]) {
		t.Fatal("c1 is older than the newest two batches of the turn: compacted")
	}
	if out[10].Content != "small" {
		t.Fatalf("a small result changed: %q", out[10].Content)
	}
	if out[12].Content != big || out[14].Content != big {
		t.Fatal("the newest two batches of the current turn stay whole")
	}
	if stat.results != 3 || len(stat.tools) != 3 {
		t.Fatalf("stat = %+v, want 3 compacted", stat)
	}
}

// The hallucination retry appends a synthetic user message; the turn's start
// does not move, so the turn keeps its own evidence (review 3b30 New-3).
func TestCompactTurnToolResults_SyntheticUserKeepsBoundary_646(t *testing.T) {
	msgs := []chat.Message{{Role: "user", Content: "now"}}
	msgs = append(msgs, batch("c1", "web_fetch", big)...)
	msgs = append(msgs, chat.Message{Role: "assistant", Content: "draft"}, chat.Message{Role: "user", Content: "fix your claims"})
	msgs = append(msgs, batch("c2", "web_fetch", "x")...)
	out, _ := compactTurnToolResults(msgs, 0)
	if out[2].Content != big {
		t.Fatal("a synthetic user message must not turn the turn's own batches into history")
	}
}

// With no user message the whole list is the current turn (review 3b30 New-4).
func TestCompactTurnToolResults_NoUserMessage_646(t *testing.T) {
	var msgs []chat.Message
	msgs = append(msgs, batch("c1", "web_fetch", big)...)
	msgs = append(msgs, batch("c2", "web_fetch", "x")...)
	if turnStartIndex(msgs) != 0 {
		t.Fatalf("turnStartIndex with no user message = %d, want 0", turnStartIndex(msgs))
	}
	out, _ := compactTurnToolResults(msgs, turnStartIndex(msgs))
	if out[1].Content != big {
		t.Fatal("inside the newest two batches of the current turn: whole")
	}
	msgs = append(msgs, batch("c3", "web_fetch", "y")...)
	out, _ = compactTurnToolResults(msgs, turnStartIndex(msgs))
	if !isCompacted(out[1]) {
		t.Fatal("aged out of the newest two: compacted")
	}
}

// Parallel calls in one assistant message are one batch; a short refusal
// beside a large result stays as it is.
func TestCompactTurnToolResults_ParallelBatch_646(t *testing.T) {
	msgs := []chat.Message{{Role: "user", Content: "now"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{
			{ID: "p1", Type: "function", Function: chat.FunctionCall{Name: "web_fetch"}},
			{ID: "p2", Type: "function", Function: chat.FunctionCall{Name: "get_task_status"}}}},
		{Role: "tool", ToolCallID: "p1", Name: "web_fetch", Content: big},
		{Role: "tool", ToolCallID: "p2", Name: "get_task_status", Content: "refused: repeated status read"},
	}
	msgs = append(msgs, batch("c2", "web_fetch", "x")...)
	out, _ := compactTurnToolResults(msgs, 0)
	if out[2].Content != big {
		t.Fatal("a parallel batch inside the newest two stays whole")
	}
	msgs = append(msgs, batch("c3", "web_fetch", "y")...)
	out, _ = compactTurnToolResults(msgs, 0)
	if !isCompacted(out[2]) {
		t.Fatal("the parallel batch aged out: its large result is compacted")
	}
	if out[3].Content != "refused: repeated status read" {
		t.Fatalf("a short refusal stays as it is: %q", out[3].Content)
	}
}

// The limit is in runes, and <n> reports runes.
func TestCompactTurnToolResults_RuneLimit_646(t *testing.T) {
	at := strings.Repeat("ž", compactOverRunes)
	over := strings.Repeat("ž", compactOverRunes+1)
	for _, c := range []struct {
		body      string
		compacted bool
	}{{at, false}, {over, true}} {
		msgs := []chat.Message{{Role: "user", Content: "earlier"}}
		msgs = append(msgs, batch("h1", "web_fetch", c.body)...)
		msgs = append(msgs, chat.Message{Role: "user", Content: "now"})
		out, _ := compactTurnToolResults(msgs, 3)
		if isCompacted(out[2]) != c.compacted {
			t.Fatalf("%d runes: compacted=%v, want %v", utf8.RuneCountInString(c.body), isCompacted(out[2]), c.compacted)
		}
		if c.compacted && (!strings.Contains(out[2].Content, "returned 6001 characters") || !utf8.ValidString(out[2].Content)) {
			t.Fatalf("marker must report runes and stay valid UTF-8: %q", out[2].Content[:120])
		}
	}
}

// sizeRecordingProvider answers with one tool call per call until it has made
// `steps` of them, then with a final reply, recording each request's bytes.
type sizeRecordingProvider struct {
	ctxCapturingProvider
	steps int
	sizes []int
}

func (p *sizeRecordingProvider) answer(msgs []chat.Message) *chat.ChatResponse {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	p.sizes = append(p.sizes, n)
	r := &chat.ChatResponse{Model: "m"}
	msg := chat.Message{Role: "assistant", Content: "done"}
	if len(p.sizes) <= p.steps {
		msg = chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "c" + strings.Repeat("x", len(p.sizes)), Type: "function",
			Function: chat.FunctionCall{Name: "mcp__srv__big", Arguments: "{}"}}}}
	}
	r.Choices = append(r.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: msg})
	return r
}
func (p *sizeRecordingProvider) CompleteWithTools(_ context.Context, msgs []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return p.answer(msgs), nil
}
func (p *sizeRecordingProvider) CompleteWithToolsStream(_ context.Context, msgs []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return p.answer(msgs), nil
}

// Through the real turn loop, both entry points: five 25 KB tool steps. Before
// the change the sixth call carried all five results (~125 KB of tool content).
func TestTurnPromptStaysBounded_646(t *testing.T) {
	for _, stream := range []bool{false, true} {
		p := &sizeRecordingProvider{steps: 5}
		a := NewAgent(p, nil, nil, nil, nil, WithMaxIterations(10), WithMCPManager(&stubMCP{out: strings.Repeat("r", 25000)}))
		req := Request{Messages: []chat.Message{{Role: "user", Content: "go"}}, Project: "assistant"}
		var res Result
		if stream {
			res = a.ProcessStreaming(context.Background(), req, func(string) {})
		} else {
			res = a.Process(context.Background(), req)
		}
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		if len(p.sizes) != 6 {
			t.Fatalf("stream=%v: calls = %d, want 6", stream, len(p.sizes))
		}
		if last := p.sizes[5]; last > 80000 {
			t.Fatalf("stream=%v: sixth call sent %d bytes of content; want under 80000 (sizes %v)", stream, last, p.sizes)
		}
		// The saved turn keeps full results: compaction applies to what is sent.
		full := 0
		for _, m := range res.Messages {
			if m.Role == "tool" && len(m.Content) >= 25000 {
				full++
			}
		}
		if full != 5 {
			t.Fatalf("stream=%v: Result.Messages must keep full results, got %d of 5", stream, full)
		}
	}
}
