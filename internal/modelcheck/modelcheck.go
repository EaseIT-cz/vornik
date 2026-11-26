// Package modelcheck checks that a model answers before it is saved (GitHub
// #881, project and swarm registry design §12.2 C1): one completion through
// the instance's router with one tool offered and router fallbacks off. An
// error refuses with the provider's reason; an answer served by another model
// refuses; an answer without the tool call passes with a warning, because
// agent roles need tool calls and a text answer proves nothing about them.
package modelcheck

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/registry"
)

// Timeout bounds one check, as the earlier reachability probe did.
const Timeout = 15 * time.Second

// cacheFor is how long a pass is trusted; a refusal is never cached.
const cacheFor = 10 * time.Minute

// parallel is how many models CheckAll probes at once.
const parallel = 4

// Completer is the one call a check needs.
type Completer interface {
	CompleteWithTools(ctx context.Context, messages []chat.Message, tools []chat.Tool) (*chat.ChatResponse, error)
}

// Router pins a completion to one model, as the chat router does.
type Router interface {
	WithModel(model string) Completer
}

// Result is a check's answer. OK with a Warning still saves.
type Result struct {
	OK       bool
	Warning  string
	Reason   string
	Duration time.Duration
}

// Checker runs checks and remembers passes.
type Checker struct {
	Provider Router
	// RouteOf names the router route that serves a model, "" when none. It
	// keys the cache and explains a failure; it never refuses on its own.
	RouteOf func(model string) string
	now     func() time.Time

	mu     sync.Mutex
	passes map[string]time.Time
}

var probeTool = chat.Tool{Type: "function", Function: chat.ToolFunction{
	Name:        "get_time",
	Description: "Returns the current time.",
	Parameters:  []byte(`{"type":"object","properties":{}}`),
}}

var probeMessages = []chat.Message{
	{Role: "system", Content: "You check that a model can call tools. Call the get_time tool now."},
	{Role: "user", Content: "Call the get_time tool."},
}

func (c *Checker) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Checker) route(model string) string {
	if c.RouteOf == nil {
		return ""
	}
	return c.RouteOf(model)
}

// Check probes model once, or answers from a pass cached in the last 10
// minutes for the same model and route.
func (c *Checker) Check(ctx context.Context, model string) Result {
	route := c.route(model)
	key := model + "@" + route
	c.mu.Lock()
	if t, ok := c.passes[key]; ok && c.clock().Sub(t) < cacheFor {
		c.mu.Unlock()
		return Result{OK: true}
	}
	c.mu.Unlock()

	start := c.clock()
	r := c.probe(ctx, model, route)
	r.Duration = c.clock().Sub(start)
	if r.OK {
		c.mu.Lock()
		if c.passes == nil {
			c.passes = map[string]time.Time{}
		}
		now := c.clock()
		for k, t := range c.passes { // sweep expired passes, so the map stays small
			if now.Sub(t) >= cacheFor {
				delete(c.passes, k)
			}
		}
		c.passes[key] = now
		c.mu.Unlock()
	}
	return r
}

func (c *Checker) probe(ctx context.Context, model, route string) Result {
	if c.Provider == nil {
		return Result{Reason: "no model router is configured"}
	}
	ctx, cancel := context.WithTimeout(chat.WithoutModelFallback(ctx), Timeout)
	defer cancel()
	resp, err := c.Provider.WithModel(model).CompleteWithTools(ctx, probeMessages, []chat.Tool{probeTool})
	if err != nil {
		if route == "" {
			return Result{Reason: fmt.Sprintf("no route serves %s: %v", model, err)}
		}
		return Result{Reason: fmt.Sprintf("%s did not answer: %v", model, err)}
	}
	if resp == nil || len(resp.Choices) == 0 {
		return Result{Reason: fmt.Sprintf("%s answered with nothing", model)}
	}
	if served := strings.TrimSpace(resp.Model); served != "" && !sameModel(served, model) {
		return Result{Reason: fmt.Sprintf("%s was served by %s instead", model, served)}
	}
	if len(resp.Choices[0].Message.ToolCalls) == 0 {
		return Result{OK: true, Warning: fmt.Sprintf("%s answered without calling the offered tool; agent roles need tool calls", model)}
	}
	return Result{OK: true}
}

// sameModel tolerates a provider that reports the model with a prefix or
// suffix of its own (a region, a version tag), but not a different model.
func sameModel(served, asked string) bool {
	return served == asked || strings.Contains(served, asked) || strings.Contains(asked, served)
}

// CheckAll checks each distinct non-empty model, at most 4 at once.
func (c *Checker) CheckAll(ctx context.Context, models []string) map[string]Result {
	out := map[string]Result{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)
	seen := map[string]bool{}
	for _, m := range models {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		wg.Add(1)
		sem <- struct{}{}
		go func(m string) {
			defer wg.Done()
			defer func() { <-sem }()
			r := c.Check(ctx, m)
			mu.Lock()
			out[m] = r
			mu.Unlock()
		}(m)
	}
	wg.Wait()
	return out
}

// FromProvider adapts the daemon's chat provider. A provider that cannot be
// pinned to one model is refused, rather than probing its default model and
// calling that a pass.
func FromProvider(p chat.Provider) Router { return providerRouter{p: p} }

type providerRouter struct{ p chat.Provider }

func (r providerRouter) WithModel(model string) Completer {
	if o, ok := r.p.(chat.ModelOverridable); ok {
		return o.WithModel(model)
	}
	return unpinnable{}
}

type unpinnable struct{}

func (unpinnable) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	return nil, fmt.Errorf("the chat provider cannot be pinned to one model, so the model was not checked")
}

// SwarmFileModels is every model and fallback a swarm file's roles name
// themselves. It parses the file, not the registry: a loaded swarm also
// carries the models its tiers resolved to, which the file never saved.
// Unparsable content names none.
func SwarmFileModels(raw []byte, name string) []string {
	sw, err := registry.ParseSwarmMarkdown(raw, filepath.Base(name))
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range sw.Roles {
		for _, m := range []string{r.Model, r.ModelFallback} {
			if m != "" {
				out = append(out, m)
			}
		}
	}
	return out
}

// RenderedModels is every model the swarm files among rendered template
// files (path -> content) name.
func RenderedModels(rendered map[string]string) []string {
	var out []string
	for path, body := range rendered {
		if strings.HasSuffix(path, ".md") && strings.Contains("/"+path, "/swarms/") {
			out = append(out, SwarmFileModels([]byte(body), path)...)
		}
	}
	return out
}
