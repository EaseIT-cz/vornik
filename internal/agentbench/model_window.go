package agentbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Model context-window discovery.
//
// WHY THIS EXISTS. `context_size` shapes every agent's compaction behaviour:
// entrypoint.sh budgets the conversation at
// (context - max_tokens - 2048) * 3 bytes/token * 80%. A model with no
// `agent_llm.model_limits` entry silently inherits the daemon-wide global, and
// nothing anywhere reports the substitution.
//
// That inheritance caused the 2026-07-12 context-overflow incident and then
// caused it again in the 2026-08-16 long-horizon arm, where
// Qwen/Qwen3.8-27B-FP8 inherited 100000 against a real 32768 — a 3.05x
// over-estimate producing 14 of the arm's 73 failures.
//
// WHY IT IS DISCOVERY RATHER THAN A CONSTANT. The window is operator-
// configurable: this vLLM deployment can be raised to 500K or 1M, which
// Qwen3.8 supports. Pinning today's 32768 would be wrong the moment the server
// changes, and wrong in the more insidious direction — the agent would compact
// at 32K against a 1M window and discard context it was entitled to use.
// Under-estimating wastes capability as surely as over-estimating overflows.
//
// WHY THE RESULT IS PROVENANCE, NOT JUST A CHECK. Two arms served by a 32K
// server and a 1M server are not comparable even under byte-identical config.
// That is the three-artifact problem HarnessBuild/DaemonBuild were added to
// close: an axis that moves the numbers while nothing declares the arm changed.

// reMaxModelLen extracts the window from an OpenAI-compatible server's
// refusal. vLLM phrases it as
//
//	max_tokens=99000000 cannot be greater than max_model_len=max_total_tokens=32768
//
// and some builds emit only the max_model_len clause. Anchoring on
// `max_model_len=` and taking the LAST `=`-separated integer handles both
// without being tuned to one value or one order of magnitude.
var reMaxModelLen = regexp.MustCompile(`max_model_len=(?:max_total_tokens=)?(\d+)`)

// parseMaxModelLen pulls the server-reported window out of an error body.
// Returns ok=false when the body says nothing about the window, which is
// distinct from the server reporting a window of zero.
func parseMaxModelLen(body string) (int, bool) {
	m := reMaxModelLen.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// DiscoverModelWindow asks an OpenAI-compatible endpoint for its context
// window by requesting an impossible number of output tokens and reading the
// limit out of the refusal.
//
// This is deliberate rather than a fallback: /v1/models does not carry
// max_model_len on every build, and a probe that the server ANSWERS is worth
// more than a field that may be absent. It costs one 400 response and no
// tokens — the request is refused before any generation happens.
//
// Returns 0 with a nil error when the endpoint answers but says nothing about
// a window: that is "undiscovered", not "zero", and CheckConfiguredWindow
// treats it as inconclusive rather than as a mismatch.
func DiscoverModelWindow(ctx context.Context, endpoint, apiKey, model string) (int, error) {
	// Ollama does not refuse an impossible max_tokens: it clamps and
	// generates, so the probe below would only run into its deadline. Ask it
	// directly first (benchmark LLD §12.11.3, amended 2026-09-26).
	if n, isOllama, err := discoverOllamaWindow(ctx, endpoint, apiKey, model); isOllama {
		return n, err
	}
	body := fmt.Sprintf(`{"model":%q,"max_tokens":99000000,"messages":[{"role":"user","content":"x"}]}`, model)

	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		endpoint+"/chat/completions", bytes.NewReader([]byte(body)))
	if err != nil {
		return 0, fmt.Errorf("model-window probe: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("model-window probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap the read: a misconfigured endpoint could stream something large,
	// and the limit clause is always near the front.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return 0, fmt.Errorf("model-window probe: read body: %w", err)
	}

	n, ok := parseMaxModelLen(string(raw))
	if !ok {
		return 0, nil
	}
	return n, nil
}

// WindowVerdictKind labels the outcome of comparing configured against
// discovered.
type WindowVerdictKind string

const (
	// WindowOK — configured matches what the server reports.
	WindowOK WindowVerdictKind = "ok"
	// WindowOverEstimate — configured exceeds the server's window. The agent
	// budgets a conversation the model cannot hold and overflows mid-step.
	WindowOverEstimate WindowVerdictKind = "over_estimate"
	// WindowUnderEstimate — configured is well below the server's window. Safe,
	// but the agent compacts earlier than it needs to and discards usable
	// context.
	WindowUnderEstimate WindowVerdictKind = "under_estimate"
	// WindowUnconfigured — no model_limits entry, so the model silently
	// inherits the daemon-wide global. The 2026-07-12 trap.
	WindowUnconfigured WindowVerdictKind = "unconfigured"
	// WindowUndiscovered — the probe could not establish a window. Says
	// nothing about the configuration either way.
	WindowUndiscovered WindowVerdictKind = "undiscovered"
)

// underEstimateTolerance is how far below the server's window a configured
// value may sit before it is worth reporting. A little headroom is normal and
// often deliberate; a third of the window is not.
const underEstimateTolerance = 0.75

// WindowVerdict is one model's configured-vs-observed result.
type WindowVerdict struct {
	Model      string
	Configured int
	Discovered int
	Verdict    WindowVerdictKind
	// Fatal is true when the run must not proceed. Only the two directions
	// that silently corrupt a run set it: an over-estimate (overflow) and an
	// unconfigured model (inherits the global, invisibly).
	Fatal   bool
	Message string
}

// CheckConfiguredWindow compares a model's configured context size against the
// window its server actually reports.
//
// Asymmetric by design. An over-estimate overflows mid-step and is fatal. An
// under-estimate merely wastes capability and may be a deliberate constraint
// on the arm, so it warns. An unconfigured model is fatal despite looking
// harmless: inheriting the global is exactly the silent substitution that has
// now caused two incidents, and it cannot be noticed at config-read time.
func CheckConfiguredWindow(model string, configured, discovered int) WindowVerdict {
	v := WindowVerdict{Model: model, Configured: configured, Discovered: discovered}

	switch {
	case discovered <= 0:
		v.Verdict = WindowUndiscovered
		v.Message = fmt.Sprintf("model %q: context window could not be discovered from the endpoint; "+
			"configured value %d is unverified", model, configured)
	case configured <= 0:
		v.Verdict, v.Fatal = WindowUnconfigured, true
		v.Message = fmt.Sprintf("model %q has no agent_llm.model_limits entry and silently inherits the "+
			"daemon-wide global; the endpoint reports %d. An inherited window is invisible at config "+
			"read and caused the 2026-07-12 overflow incident — set it explicitly", model, discovered)
	case configured > discovered:
		v.Verdict, v.Fatal = WindowOverEstimate, true
		v.Message = fmt.Sprintf("model %q is configured for a %d-token window but the endpoint serves "+
			"%d. The agent budgets a conversation the model cannot hold and overflows mid-step",
			model, configured, discovered)
	case float64(configured) < float64(discovered)*underEstimateTolerance:
		v.Verdict = WindowUnderEstimate
		v.Message = fmt.Sprintf("model %q is configured for %d tokens but the endpoint serves %d; "+
			"the agent will compact earlier than necessary and discard usable context. Not fatal — "+
			"this may be a deliberate constraint", model, configured, discovered)
	default:
		v.Verdict = WindowOK
		v.Message = fmt.Sprintf("model %q: configured %d, endpoint serves %d", model, configured, discovered)
	}
	return v
}

// ErrOllamaModelNotLoaded means the endpoint is Ollama and the model is not loaded,
// so its served window cannot be read without loading it and generating.
// "Undiscovered", not a mismatch.
var ErrOllamaModelNotLoaded = errors.New("model not loaded on the Ollama server; its served window is unknown until it is")

// ollamaRoot strips ONE trailing "/v1" (both Ollama roots serve the OpenAI
// surface at host/v1). Anything else is left alone; its /api/ps then 404s and
// the refusal probe runs.
func ollamaRoot(endpoint string) string {
	e := strings.TrimRight(endpoint, "/")
	return strings.TrimSuffix(e, "/v1")
}

// discoverOllamaWindow reads the served window from Ollama's GET /api/ps.
// isOllama is false for anything that does not answer like Ollama, and the
// caller then runs the refusal probe.
func discoverOllamaWindow(ctx context.Context, endpoint, apiKey, model string) (n int, isOllama bool, err error) {
	root := ollamaRoot(endpoint)
	var ps struct {
		Models *[]struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			Digest        string `json:"digest"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if !ollamaGetJSON(ctx, root+"/api/ps", apiKey, &ps) || ps.Models == nil {
		return 0, false, nil
	}
	for _, m := range *ps.Models {
		// Every entry must look like Ollama's, or this is some other server
		// that happens to have a "models" list.
		if (m.Name == "" && m.Model == "") || m.ContextLength <= 0 {
			return 0, false, nil
		}
	}
	for _, m := range *ps.Models {
		if m.Name == model || m.Model == model {
			return m.ContextLength, true, nil
		}
	}
	// An alias whose original is already loaded is listed under the
	// original's name; the two share a digest (§12.11.3, aliases).
	if digest := ollamaDigest(ctx, root, apiKey, model); digest != "" {
		for _, m := range *ps.Models {
			if m.Digest == digest {
				return m.ContextLength, true, nil
			}
		}
	}
	// Not listed: confirm it IS Ollama before refusing to probe.
	var ver struct {
		Version string `json:"version"`
	}
	if !ollamaGetJSON(ctx, root+"/api/version", apiKey, &ver) || ver.Version == "" {
		return 0, false, nil
	}
	return 0, true, fmt.Errorf("model-window probe: %q: %w", model, ErrOllamaModelNotLoaded)
}

// ollamaDigest returns the model's digest from GET /api/tags, or "" when it
// cannot be read (the caller then reports "undiscovered", never a guess).
func ollamaDigest(ctx context.Context, root, apiKey, model string) string {
	var tags struct {
		Models []struct {
			Name   string `json:"name"`
			Model  string `json:"model"`
			Digest string `json:"digest"`
		} `json:"models"`
	}
	if !ollamaGetJSON(ctx, root+"/api/tags", apiKey, &tags) {
		return ""
	}
	for _, t := range tags.Models {
		if t.Name == model || t.Model == model {
			return t.Digest
		}
	}
	return ""
}

// ollamaGetJSON GETs url and decodes a 200 JSON body into v. False on any
// failure: the caller treats that as "not Ollama".
func ollamaGetJSON(ctx context.Context, url, apiKey string, v any) bool {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(v) == nil
}
