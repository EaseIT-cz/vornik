package hermes

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// retryTemperature is the sampling temperature for a step's second and
// third attempts. llama.cpp serves at temperature 0 so a first attempt is
// reproducible; a retry at the same temperature would repeat it exactly.
const retryTemperature = 0.7

// bodyWatch watches completion request bodies for one text: the H3f arm's
// check that a forgotten fact is absent from the prefetch block, which
// reaches the model only inside the request (design 24, 0.8.0).
type bodyWatch struct {
	wmu  sync.Mutex
	text string
	seen bool
}

// Watch starts watching for text and forgets what was seen before.
func (b *bodyWatch) Watch(text string) {
	b.wmu.Lock()
	b.text, b.seen = text, false
	b.wmu.Unlock()
}

// WatchSeen reports whether a request since Watch carried the text.
func (b *bodyWatch) WatchSeen() bool {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	return b.seen
}

func (b *bodyWatch) inspect(body []byte) {
	b.wmu.Lock()
	if b.text != "" && bytes.Contains(body, []byte(b.text)) {
		b.seen = true
	}
	b.wmu.Unlock()
}

// ToolRecorder sits between Hermes and llama.cpp. It records the tools
// Hermes offers the model (Hermes's session store does not keep them),
// re-samples retries (lane design, As built) and bounds every reply
// (amendment 2026-10-14, GitHub #380).
type ToolRecorder struct {
	bodyWatch
	evidence  callEvidence
	proxy     *httputil.ReverseProxy
	maxTokens int

	mu        sync.Mutex
	attempt   int
	toolsSeen map[string]bool
}

// NewToolRecorder forwards to upstream, an http://host:port base URL, and
// caps every completion request at maxTokens reply tokens (positive).
func NewToolRecorder(upstream string, maxTokens int) *ToolRecorder {
	u, err := url.Parse(upstream)
	if err != nil {
		panic(err)
	}
	if maxTokens <= 0 {
		panic("NewToolRecorder: maxTokens must be positive")
	}
	r := &ToolRecorder{proxy: httputil.NewSingleHostReverseProxy(u), maxTokens: maxTokens, attempt: 1, toolsSeen: map[string]bool{}}
	r.proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
			return nil
		}
		resp.Body = &observingModelBody{ReadCloser: resp.Body, p: newModelCallParser(&r.evidence, strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream"))}
		return nil
	}
	return r
}

// SetAttempt tells the recorder which attempt of a step is running.
func (r *ToolRecorder) SetAttempt(n int) {
	r.mu.Lock()
	r.attempt = n
	r.mu.Unlock()
}

// ToolsSeen lists every tool name offered in any completion request, sorted.
func (r *ToolRecorder) ToolsSeen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.toolsSeen))
	for n := range r.toolsSeen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (r *ToolRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "chat/completions") {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.inspect(body)
		r.evidence.chatInput(body)
		body = r.observe(body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	r.proxy.ServeHTTP(w, req)
}

// observe records the offered tools, bounds the reply, and, on a retry, sets
// the sampling temperature and a per-attempt seed. A body that is not a JSON
// object is forwarded unchanged (llama.cpp refuses it; its -n is the bound).
func (r *ToolRecorder) observe(body []byte) []byte {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil || req == nil {
		return body
	}
	var tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	_ = json.Unmarshal(req["tools"], &tools)
	r.mu.Lock()
	for _, tl := range tools {
		r.toolsSeen[tl.Function.Name] = true
	}
	attempt := r.attempt
	r.mu.Unlock()
	capReplyTokens(req, r.maxTokens)
	if attempt > 1 {
		req["temperature"], _ = json.Marshal(retryTemperature)
		req["seed"], _ = json.Marshal(attempt)
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

// replyBoundKeys are the names llama.cpp b11610 reads as one request's reply
// bound (tools/server/server-schema.cpp: n_predict, aliases max_tokens and
// max_completion_tokens). A request's bound replaces the server's -n there,
// and Hermes retries a truncated tool call with max_tokens 4096*2^n, so the
// lane's bound is enforced here, on every request.
var replyBoundKeys = []string{"max_tokens", "max_completion_tokens", "n_predict"}

// capReplyTokens leaves exactly one bound, max_tokens: the smallest
// non-negative integer the request named, at most maxTokens; maxTokens when
// it named none (or only -1, "unlimited", or a non-number).
func capReplyTokens(req map[string]json.RawMessage, maxTokens int) {
	bound := maxTokens
	for _, k := range replyBoundKeys {
		raw, ok := req[k]
		if !ok {
			continue
		}
		delete(req, k)
		var n int
		if json.Unmarshal(raw, &n) == nil && n >= 0 && n < bound {
			bound = n
		}
	}
	req["max_tokens"], _ = json.Marshal(bound)
}
