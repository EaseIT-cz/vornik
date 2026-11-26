package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
)

// ReasoningSupport is what a sub-provider kind does with a requested
// reasoning level (reasoning control design B4, GitHub #869).
type ReasoningSupport string

const (
	// ReasoningSent means the route honours the level in its own field; for
	// off that can be sending nothing (Claude: no thinking block).
	ReasoningSent ReasoningSupport = "sent"
	// ReasoningApproximated means a nearer level than asked is sent.
	ReasoningApproximated ReasoningSupport = "approximated"
	// ReasoningUnsupported means the kind sends nothing for the level.
	ReasoningUnsupported ReasoningSupport = "unsupported"
	// ReasoningRefused means the model rejected the field and the call was
	// retried without it. A runtime answer, never a table entry.
	ReasoningRefused ReasoningSupport = "refused"
)

// allLevels maps every level to one answer.
func allLevels(s ReasoningSupport) map[string]ReasoningSupport {
	return map[string]ReasoningSupport{
		ReasoningEffortOff: s, ReasoningEffortLow: s, ReasoningEffortMedium: s, ReasoningEffortHigh: s,
	}
}

// withOff maps off to one answer and the other levels to another.
func withOff(off, rest ReasoningSupport) map[string]ReasoningSupport {
	m := allLevels(rest)
	m[ReasoningEffortOff] = off
	return m
}

// reasoningTable is the one declaration of what each sub-provider kind does
// with each level. A kind that is not here cannot be registered
// (SubProviders.Add), so a provider added later must declare its mapping.
//
// It states what the code does: codex-cli ignores the effort flag, so it
// reads unsupported.
var reasoningTable = map[string]map[string]ReasoningSupport{
	// OpenAI-compatible routes write reasoning_effort ("none" for off).
	"http":         allLevels(ReasoningSent),
	"openrouter":   allLevels(ReasoningSent),
	"ollama_cloud": allLevels(ReasoningSent),
	"vertex":       allLevels(ReasoningSent),
	// Bedrock writes reasoning_effort in AdditionalModelRequestFields.
	"bedrock": allLevels(ReasoningSent),
	// Claude: a thinking budget per level, off sends no block. A budget the
	// request's max_tokens cannot hold is clamped or dropped at run time
	// (claudeBudgetForLevel); that is not counted.
	"claude-subscription": allLevels(ReasoningSent),
	// Codex and the Claude CLI have no "off": their lowest effort is sent.
	"codex-subscription": withOff(ReasoningApproximated, ReasoningSent),
	"claude-cli":         withOff(ReasoningApproximated, ReasoningSent),
	// codex-cli ignores the effort flag (design A2).
	"codex-cli": allLevels(ReasoningUnsupported),
}

// ReasoningSupportFor reports what kind does with level, and false when the
// kind or the level is not declared.
func ReasoningSupportFor(kind, level string) (ReasoningSupport, bool) {
	levels, ok := reasoningTable[kind]
	if !ok {
		return "", false
	}
	s, ok := levels[level]
	return s, ok
}

// SubProviders is the router's set of sub-providers by kind. Its map is
// unexported, so a provider is registered only through Add, which refuses a
// kind the reasoning table does not declare.
type SubProviders struct {
	m map[string]Provider
}

// NewSubProviders returns an empty set.
func NewSubProviders() *SubProviders {
	return &SubProviders{m: map[string]Provider{}}
}

// Add registers p under kind, refusing a kind with no reasoning-table entry:
// every provider states how it maps the reasoning levels before it can serve.
func (s *SubProviders) Add(kind string, p Provider) error {
	if _, ok := reasoningTable[kind]; !ok {
		return fmt.Errorf("chat: sub-provider kind %q has no reasoning-table entry (internal/chat/reasoning_table.go); declare how it maps off, low, medium and high, or that it cannot", kind)
	}
	s.m[kind] = p
	return nil
}

// Map returns a copy of the registered providers, so writing to it cannot
// register one.
func (s *SubProviders) Map() map[string]Provider {
	return maps.Clone(s.m)
}

// Per-route mappings of a request's level (design B4). Each returns the
// route's own construction-time value when the request carries no level, so
// a request without one is sent as before, and a request level overrides it.

// codexEffort is the Responses reasoning.effort for codex-subscription. Codex
// has no off, so off sends its lowest effort.
func codexEffort(ctx context.Context, route string) string {
	return lowestForOff(ReasoningEffortFromContext(ctx), route)
}

// cliEffort is CLAUDE_CODE_EFFORT_LEVEL for the Claude CLI, mapped the same
// way as Codex's effort.
func cliEffort(ctx context.Context, route string) string {
	return lowestForOff(ReasoningEffortFromContext(ctx), route)
}

func lowestForOff(level, route string) string {
	switch level {
	case "":
		return route
	case ReasoningEffortOff:
		return ReasoningEffortLow
	default:
		return level
	}
}

// claudeBudgetForLevel is the Anthropic thinking budget for a level: 0 for
// off (no thinking block), else 1024 / 4096 / 16384, clamped to half of
// maxTokens because Anthropic requires budget_tokens below max_tokens, and 0
// when even 1024 does not fit under that half.
func claudeBudgetForLevel(level string, maxTokens int) int {
	budget := map[string]int{ReasoningEffortLow: 1024, ReasoningEffortMedium: 4096, ReasoningEffortHigh: 16384}[level]
	if budget > maxTokens/2 {
		budget = maxTokens / 2
	}
	if budget < 1024 {
		return 0
	}
	return budget
}

// reasoningRefusals remembers, per endpoint and model, that the model refused
// the reasoning field (design B4, round-one review H3). It lasts until the
// daemon restarts, so a model that rejects the field costs one retried call,
// not one per request.
var reasoningRefusals sync.Map

func reasoningRefusalKey(endpoint, model string) string { return endpoint + "|" + model }

func reasoningRefused(endpoint, model string) bool {
	_, ok := reasoningRefusals.Load(reasoningRefusalKey(endpoint, model))
	return ok
}

// rememberReasoningRefusal records a refusal and reports whether it is new,
// so the caller warns once.
func rememberReasoningRefusal(endpoint, model string) bool {
	_, loaded := reasoningRefusals.LoadOrStore(reasoningRefusalKey(endpoint, model), true)
	return !loaded
}

// reasoningOffAsLow remembers, per endpoint and model, that the model
// refused the value "none", so off is sent as "low" (design B17). Like
// reasoningRefusals it lasts until restart.
var reasoningOffAsLow sync.Map

func reasoningOffAsLowFor(endpoint, model string) bool {
	_, ok := reasoningOffAsLow.Load(reasoningRefusalKey(endpoint, model))
	return ok
}

// rememberReasoningOffAsLow records it and reports whether it is new, so
// the caller warns once.
func rememberReasoningOffAsLow(endpoint, model string) bool {
	_, loaded := reasoningOffAsLow.LoadOrStore(reasoningRefusalKey(endpoint, model), true)
	return !loaded
}

// isReasoningRefusal reports whether err is a 400 that names the reasoning
// field: the structured error.param first, then the message text, for
// providers that send text alone. A 400 about anything else is not one.
func isReasoningRefusal(err error) bool {
	var ge *GatewayError
	if !errors.As(err, &ge) || ge.Status != 400 {
		return false
	}
	var body struct {
		Error struct {
			Param string `json:"param"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(ge.Body), &body) == nil && body.Error.Param != "" {
		return body.Error.Param == "reasoning_effort"
	}
	return strings.Contains(strings.ToLower(ge.Message+" "+ge.Body), "reasoning_effort")
}
