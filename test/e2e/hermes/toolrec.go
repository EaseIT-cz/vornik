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
// Hermes offers the model (Hermes's session store does not keep them) and
// re-samples retries (lane design, As built).
type ToolRecorder struct {
	bodyWatch
	proxy *httputil.ReverseProxy

	mu        sync.Mutex
	attempt   int
	toolsSeen map[string]bool
}

// NewToolRecorder forwards to upstream, an http://host:port base URL.
func NewToolRecorder(upstream string) *ToolRecorder {
	u, err := url.Parse(upstream)
	if err != nil {
		panic(err)
	}
	return &ToolRecorder{proxy: httputil.NewSingleHostReverseProxy(u), attempt: 1, toolsSeen: map[string]bool{}}
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
		body = r.observe(body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	r.proxy.ServeHTTP(w, req)
}

// observe records the offered tools and, on a retry, sets the sampling
// temperature and a per-attempt seed. A body that is not a JSON object is
// forwarded unchanged.
func (r *ToolRecorder) observe(body []byte) []byte {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
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
	if attempt <= 1 {
		return body
	}
	req["temperature"], _ = json.Marshal(retryTemperature)
	req["seed"], _ = json.Marshal(attempt)
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}
