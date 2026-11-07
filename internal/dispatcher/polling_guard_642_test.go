package dispatcher

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// GitHub #642 (incident 2026-11-04 22:43–22:50, Telegram): with a completion
// follow-up registered, the dispatcher polled list_tasks and get_task_status
// in runs of 8 to 21 identical calls and hit the tool iteration cap twice.
// A status read cannot change within a turn, and the follow-up already tells
// the user when the task ends (dispatcher-wait-for-data design). The guard
// refuses a read-only status tool after maxIdenticalStatusReads identical
// calls in one turn, and tells the model to answer.
//
// Found at: production (operator observation in the Telegram chat).
// Regression test at: unit, the dispatcher's own tool loop against a scripted
// model over HTTP, for both the plain and the streaming loop.
func pollingModel(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var toolMsgs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []chat.Message `json:"messages"`
			Stream   bool           `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		toolMsgs = toolMsgs[:0]
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolMsgs = append(toolMsgs, m.Content)
			}
		}
		mu.Unlock()
		call := `{"id":"tc","type":"function","function":{"name":"get_task_status","arguments":"{\"task_id\":\"task_x\"}"}}`
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"id":"r","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,`+strings.TrimPrefix(call, "{")+`]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+call+`]},"finish_reason":"tool_calls"}]}`)
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), toolMsgs...)
	}
}

func checkPollingRefused(t *testing.T, toolMsgs []string) {
	t.Helper()
	refused := 0
	for i, m := range toolMsgs {
		if strings.Contains(m, "not polling") {
			refused++
			if i < maxIdenticalStatusReads {
				t.Fatalf("call %d was refused; the first %d identical reads must run", i+1, maxIdenticalStatusReads)
			}
		}
	}
	if refused == 0 {
		t.Fatalf("no polling call was refused in %d tool results: %q", len(toolMsgs), toolMsgs)
	}
}

func TestAgent_Process_RefusesRepeatedStatusPolling_642(t *testing.T) {
	srv, seen := pollingModel(t)
	defer srv.Close()
	agent := NewAgent(chat.NewClient(srv.URL, "k", "m"), nil, nil, nil, nil, WithMaxIterations(6))
	agent.Process(context.Background(), Request{Messages: []chat.Message{{Role: "user", Content: "is it done?"}}, Project: "p1", AllowedProjects: []string{"*"}})
	checkPollingRefused(t, seen())
}

func TestAgent_ProcessStreaming_RefusesRepeatedStatusPolling_642(t *testing.T) {
	srv, seen := pollingModel(t)
	defer srv.Close()
	agent := NewAgent(chat.NewClient(srv.URL, "k", "m"), nil, nil, nil, nil, WithMaxIterations(6))
	agent.ProcessStreaming(context.Background(), Request{Messages: []chat.Message{{Role: "user", Content: "is it done?"}}, Project: "p1", AllowedProjects: []string{"*"}}, func(string) {})
	checkPollingRefused(t, seen())
}

func TestStatusReadGuard_642(t *testing.T) {
	g := newStatusReadGuard()
	for i := 0; i < maxIdenticalStatusReads; i++ {
		if _, refused := g.check("get_task_status", `{"task_id":"a"}`); refused {
			t.Fatalf("identical read %d refused", i+1)
		}
	}
	if _, refused := g.check("get_task_status", `{"task_id":"a"}`); !refused {
		t.Fatal("an identical read past the limit ran")
	}
	// Key order and spacing do not make a new read (review 0c05 F7).
	if _, refused := g.check("get_task_status", `{ "task_id" : "a" }`); !refused {
		t.Fatal("the same read with different spacing ran")
	}
	// A state-changing call makes the next identical read a new question
	// (review 0c05 F5).
	g.check("cancel_task", `{"task_id":"a"}`)
	if _, refused := g.check("get_task_status", `{"task_id":"a"}`); refused {
		t.Fatal("a re-read after a state change was refused")
	}
	// Another task's status is a different read.
	if _, refused := g.check("get_task_status", `{"task_id":"b"}`); refused {
		t.Fatal("a read of another task was refused")
	}
	// A tool that changes state is never refused.
	for i := 0; i < 10; i++ {
		if _, refused := g.check("create_task", `{}`); refused {
			t.Fatal("a non-status tool was refused")
		}
	}
	// The per-tool total holds across arguments.
	g2 := newStatusReadGuard()
	refusedAny := false
	for i := 0; i < maxStatusReadsPerTool+1; i++ {
		if _, refused := g2.check("list_tasks", `{"status":"s`+string(rune('a'+i))+`"}`); refused {
			refusedAny = true
		}
	}
	if !refusedAny {
		t.Fatalf("%d list_tasks calls with different arguments all ran", maxStatusReadsPerTool+1)
	}
}
