package hermes

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// ResponsesStub is a scripted OpenAI Responses API server for Codex, which
// speaks only that API (agent-administered Vornik plan P8.4; probed against
// codex-cli 0.153, 2026-10-02). It follows Scripts exactly as LLMStub does:
// the first script whose Marker is in the conversation; with n
// function_call_output items present it makes step n's call, then answers
// with the final text. An MCP tool is offered inside a namespace
// (mcp__<server>) and called by its plain name with that namespace.
type ResponsesStub struct {
	Scripts  []Script
	Final    string
	mu       sync.Mutex
	failures []string
	seen     []string
}

// Failures lists script steps that could not be made.
func (s *ResponsesStub) Failures() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.failures...)
}

// ToolsSeen is the last request's tools, as namespace__name.
func (s *ResponsesStub) ToolsSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

type responsesTool struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Tools []responsesTool `json:"tools"`
}

// responsesTarget is where an offered tool lives: its namespace (for an
// MCP tool) and its plain name.
type responsesTarget struct{ ns, name string }

// offeredTools flattens the request's tools to composite names
// (namespace__name for a namespaced tool).
func offeredTools(tools []responsesTool) (map[string]responsesTarget, []string) {
	offered := map[string]responsesTarget{}
	var names []string
	for _, t := range tools {
		switch t.Type {
		case "function":
			offered[t.Name] = responsesTarget{name: t.Name}
			names = append(names, t.Name)
		case "namespace":
			for _, in := range t.Tools {
				c := t.Name + "__" + in.Name
				offered[c] = responsesTarget{ns: t.Name, name: in.Name}
				names = append(names, c)
			}
		}
	}
	return offered, names
}

// readInput is the conversation's text, how many tool results it holds and
// the last one.
func readInput(input []map[string]any) (string, int, string) {
	var text strings.Builder
	results, last := 0, ""
	for _, it := range input {
		raw, _ := json.Marshal(it)
		text.Write(raw)
		if it["type"] != "function_call_output" {
			continue
		}
		results++
		if o, ok := it["output"].(string); ok {
			last = o
		} else {
			b, _ := json.Marshal(it["output"])
			last = string(b)
		}
	}
	return text.String(), results, last
}

// ServeHTTP implements POST .../responses with a streamed reply.
func (s *ResponsesStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	var req struct {
		Input []map[string]any `json:"input"`
		Tools []responsesTool  `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offered, names := offeredTools(req.Tools)
	text, results, last := readInput(req.Input)
	s.mu.Lock()
	s.seen = names
	s.mu.Unlock()
	writeResponseItem(w, s.nextItem(text, offered, results, last))
}

// nextItem is the scripted turn: step n's call, or the final message.
func (s *ResponsesStub) nextItem(text string, offered map[string]responsesTarget, results int, last string) map[string]any {
	var script *Script
	for i := range s.Scripts {
		if strings.Contains(text, s.Scripts[i].Marker) {
			script = &s.Scripts[i]
			break
		}
	}
	final := s.Final
	switch {
	case script != nil && results < len(script.Steps):
		if item, ok := s.callFor(script.Steps[results], offered, results, last); ok {
			return item
		}
		final = "SCRIPT ERROR: tool " + script.Steps[results].ToolSuffix + " was not offered"
	case script != nil && script.FinalFrom != nil:
		final = script.FinalFrom(last)
	case script != nil:
		final = script.Final
	}
	if final == "" {
		final = "Done."
	}
	return map[string]any{"type": "message", "id": "msg_final", "role": "assistant", "status": "completed",
		"content": []map[string]any{{"type": "output_text", "text": final, "annotations": []any{}}}}
}

// callFor is step's function call, or false when no offered tool matches.
func (s *ResponsesStub) callFor(step ScriptStep, offered map[string]responsesTarget, n int, last string) (map[string]any, bool) {
	for c, tg := range offered {
		if !strings.HasSuffix(c, step.ToolSuffix) {
			continue
		}
		args := step.Args
		if step.ArgsFrom != nil {
			args = step.ArgsFrom(last)
		}
		a, _ := json.Marshal(args)
		item := map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", n), "call_id": fmt.Sprintf("call_%d", n),
			"name": tg.name, "arguments": string(a), "status": "completed"}
		if tg.ns != "" {
			item["namespace"] = tg.ns
		}
		return item, true
	}
	s.mu.Lock()
	s.failures = append(s.failures, fmt.Sprintf("step %d: no offered tool ends in %q", n, step.ToolSuffix))
	s.mu.Unlock()
	return nil, false
}

// writeResponseItem streams one output item as a complete response.
func writeResponseItem(w http.ResponseWriter, item map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(kind string, payload map[string]any) {
		payload["type"] = kind
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
	}
	ev("response.created", map[string]any{"response": map[string]any{"id": "resp_stub"}})
	ev("response.output_item.added", map[string]any{"output_index": 0, "item": item})
	ev("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	ev("response.completed", map[string]any{"response": map[string]any{"id": "resp_stub", "output": []any{item},
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
}
