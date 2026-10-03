package hermes

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// ScriptStep is one tool call a scripted model makes.
type ScriptStep struct {
	// ToolSuffix picks the tool from the request's tool list by suffix
	// (mcp__mail-read__gmail_search matches "gmail_search"), so the script
	// does not depend on the agent's naming prefix.
	ToolSuffix string
	Args       map[string]any
	// ArgsFrom, when set, builds the arguments from the previous tool
	// result's text (e.g. the task id delegate returned).
	ArgsFrom func(lastResult string) map[string]any
}

// Script is the plan for conversations whose text contains Marker.
type Script struct {
	Marker string
	Steps  []ScriptStep
	Final  string
	// FinalFrom, when set, builds the final reply from the last tool result.
	FinalFrom func(lastResult string) string
}

// LLMStub is a scripted OpenAI-compatible chat-completions server. It
// picks the first Script whose Marker appears in the conversation (else
// Steps/Final), and answers by counting the tool results already present:
// with n results it makes step n's tool call, and after the last step it
// replies with the script's final text. It never improvises, so a failure
// means the plumbing around the model broke, not the model (lane design
// §3.2). It also serves deterministic /v1/embeddings.
type LLMStub struct {
	bodyWatch
	Steps   []ScriptStep
	Final   string
	Scripts []Script
	// EmbeddingDim is the embedding width; 0 means 384.
	EmbeddingDim int

	mu        sync.Mutex
	requests  int
	failures  []string
	toolsSeen map[string]bool
	// firstTurns is the conversation text of every request that carried no
	// tool result yet: what a step was given before it read anything.
	firstTurns []string
}

// FirstTurns returns the conversation text of every request made before any
// tool result: the step's prompt as the model received it.
func (s *LLMStub) FirstTurns() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.firstTurns...)
}

// ToolsSeen lists every tool name offered in any request, sorted.
func (s *LLMStub) ToolsSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.toolsSeen))
	for n := range s.toolsSeen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Requests reports how many completions were served.
func (s *LLMStub) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Failures lists script errors (a step whose tool was not offered).
func (s *LLMStub) Failures() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.failures...)
}

// SetScripts replaces the scripts (e.g. the hostile variant for H2b).
func (s *LLMStub) SetScripts(scripts ...Script) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Scripts = scripts
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	Stream bool `json:"stream"`
}

// ServeHTTP implements POST /v1/chat/completions (any path ending in
// chat/completions) and GET /v1/models.
func (s *LLMStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/embeddings") {
		s.serveEmbeddings(w, r)
		return
	}
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
		writeJSON(w, map[string]any{"object": "list", "data": []map[string]any{{"id": "e2e-scripted", "object": "model", "context_length": 65536}}})
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "chat/completions") {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	s.inspect(body)
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var all strings.Builder
	results, last := 0, ""
	for _, m := range req.Messages {
		if m.Role == "tool" {
			results++
			last = contentText(m.Content)
		}
		all.Write(m.Content)
	}
	s.mu.Lock()
	s.requests++
	if results == 0 {
		s.firstTurns = append(s.firstTurns, all.String())
	}
	if s.toolsSeen == nil {
		s.toolsSeen = map[string]bool{}
	}
	for _, tl := range req.Tools {
		s.toolsSeen[tl.Function.Name] = true
	}
	steps, final := s.Steps, s.Final
	var finalFrom func(string) string
	for _, sc := range s.Scripts {
		if sc.Marker != "" && strings.Contains(all.String(), sc.Marker) {
			steps, final, finalFrom = sc.Steps, sc.Final, sc.FinalFrom
			break
		}
	}
	if finalFrom != nil {
		final = finalFrom(last)
	}
	s.mu.Unlock()
	msg, finish := s.reply(req, steps, results, last, final)
	resp := map[string]any{
		"id": "e2e", "object": "chat.completion", "model": req.Model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}
	if req.Stream {
		writeStream(w, resp)
		return
	}
	writeJSON(w, resp)
}

// reply is the scripted assistant message: the next step's tool call while
// steps remain, otherwise the final text.
func (s *LLMStub) reply(req chatRequest, steps []ScriptStep, results int, last, final string) (map[string]any, string) {
	msg := map[string]any{"role": "assistant", "content": final}
	finish := "stop"
	if results < len(steps) {
		step := steps[results]
		name := ""
		for _, t := range req.Tools {
			if strings.HasSuffix(t.Function.Name, step.ToolSuffix) {
				name = t.Function.Name
			}
		}
		if name == "" {
			s.mu.Lock()
			s.failures = append(s.failures, fmt.Sprintf("step %d: no offered tool ends in %q", results, step.ToolSuffix))
			s.mu.Unlock()
			msg["content"] = "SCRIPT ERROR: tool " + step.ToolSuffix + " was not offered"
		} else {
			stepArgs := step.Args
			if step.ArgsFrom != nil {
				stepArgs = step.ArgsFrom(last)
			}
			args, _ := json.Marshal(stepArgs)
			msg = map[string]any{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
				"id": fmt.Sprintf("call_%d", results), "type": "function",
				"function": map[string]any{"name": name, "arguments": string(args)},
			}}}
			finish = "tool_calls"
		}
	}
	return msg, finish
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeStream sends the reply as one SSE chunk plus [DONE], for clients that
// ask for streaming.
func writeStream(w http.ResponseWriter, resp map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	choice := resp["choices"].([]map[string]any)[0]
	delta := choice["message"]
	chunk := map[string]any{"id": resp["id"], "object": "chat.completion.chunk", "model": resp["model"],
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": choice["finish_reason"]}}}
	b, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
}

// serveEmbeddings returns deterministic unit vectors derived from each
// input's SHA-256, so identical text embeds identically.
func (s *LLMStub) serveEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var inputs []string
	if err := json.Unmarshal(req.Input, &inputs); err != nil {
		var one string
		_ = json.Unmarshal(req.Input, &one)
		inputs = []string{one}
	}
	dim := s.EmbeddingDim
	if dim <= 0 {
		dim = 384
	}
	data := make([]map[string]any, 0, len(inputs))
	for i, in := range inputs {
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": hashVector(in, dim)})
	}
	writeJSON(w, map[string]any{"object": "list", "model": req.Model, "data": data,
		"usage": map[string]any{"prompt_tokens": 1, "total_tokens": 1}})
}

func hashVector(text string, dim int) []float64 {
	v := make([]float64, dim)
	var norm float64
	for i := 0; i < dim; i++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", i, text)))
		x := float64(binary.BigEndian.Uint32(h[:4]))/float64(math.MaxUint32)*2 - 1
		v[i] = x
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] /= norm
	}
	return v
}

// contentText returns a message's content as text: a JSON string, or the
// concatenated text parts of a content array.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}
