// Package main provides the mcp-bridge binary.
//
// mcp-bridge is a small helper that runs inside agent containers, giving them
// access to MCP (Model Context Protocol) servers defined in the project config.
//
// It only relays. VORNIK_API_URL and VORNIK_PROJECT_ID name the daemon and
// the project; discover and call are forwarded to the daemon's
// /api/v1/projects/{id}/mcp/* endpoints. The daemon holds the per-project MCP
// clients, their credentials, the approval rows and the read gates (§6.4,
// §6.5 of the agent-administered design), so every call is judged there and
// the agent image needs no node/npm and no secrets.
//
// Without VORNIK_API_URL the bridge refuses every verb and starts nothing.
// The local-subprocess mode that read mcp.json and ran its own MCP manager
// inside the container, with none of the daemon's gates, was removed
// (GitHub #498; design §6.5, Amendment 2026-10-21 (#498)).
//
// Subcommands:
//
//	mcp-bridge discover
//	    Prints a JSON array of tools in OpenAI function-calling format.
//
//	mcp-bridge call <qualifiedName> <argsJSON>
//	    Calls one tool, prints its text result. Exits non-zero on error.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
)

const (
	envAPIURL    = "VORNIK_API_URL"
	envProjectID = "VORNIK_PROJECT_ID"
	// VORNIK_API_KEY is the bearer the executor injects for
	// daemon-internal callbacks. Without it, daemon auth_enabled=true
	// rejects every discover + call with 401. The executor's
	// container_scheduler sets this from agent_llm.api_key so a
	// single config knob covers chat-proxy, llm-usage, tool-audit,
	// AND mcp-bridge. Falls back to VORNIK_LLM_API_KEY (some agent
	// container shapes may inject the LLM key but not the API key)
	// for forward compat. Empty = no Authorization header sent
	// (auth_enabled=false deployments).
	envAPIKey    = "VORNIK_API_KEY"
	envLLMAPIKey = "VORNIK_LLM_API_KEY"
)

// daemonBearerToken returns the credential mcp-bridge should send
// on internal-callback requests. Empty result is valid — when
// auth_enabled=false the daemon ignores Authorization headers.
func daemonBearerToken() string {
	if v := strings.TrimSpace(os.Getenv(envAPIKey)); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(envLLMAPIKey))
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mcp-bridge: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: mcp-bridge <discover|call> [args...]")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The daemon owns the MCP clients and judges every call; the agent
	// just forwards. No daemon URL: refuse, there is no local mode (#498).
	apiURL := os.Getenv(envAPIURL)
	if apiURL == "" {
		return errNoDaemonURL
	}
	projectID := os.Getenv(envProjectID)
	if projectID == "" {
		return fmt.Errorf("%s is set but %s is empty — daemon-proxy mode requires both", envAPIURL, envProjectID)
	}
	switch os.Args[1] {
	case "discover":
		return httpDiscover(ctx, apiURL, projectID)
	case "call":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: mcp-bridge call <qualifiedName> <argsJSON>")
		}
		return httpCall(ctx, apiURL, projectID, os.Args[2], os.Args[3])
	default:
		return fmt.Errorf("unknown subcommand %q — use discover or call", os.Args[1])
	}
}

// errNoDaemonURL is the refusal without VORNIK_API_URL. It names the variable
// so the entrypoint's log says what is missing.
var errNoDaemonURL = fmt.Errorf("%s is not set: mcp-bridge only relays MCP calls to the Vornik daemon, "+
	"which holds the approvals and the credentials (the local mcp.json mode was removed, GitHub #498)", envAPIURL)

// daemonHTTP returns the base URL and HTTP client to reach the daemon.
// When VORNIK_API_URL uses the unix:// scheme — the daemon-only network
// policy, where the container has no network device and reaches the
// daemon over a bind-mounted unix socket (DaemonOnlySocketContainerPath)
// — it returns a client that dials the socket plus a synthetic
// http://unix base. Otherwise it returns the (trimmed) apiURL and a
// plain client. timeout==0 leaves the client without one.
func daemonHTTP(apiURL string, timeout time.Duration) (string, *http.Client) {
	if strings.HasPrefix(apiURL, "unix://") {
		sock := strings.TrimPrefix(apiURL, "unix://")
		dialer := &net.Dialer{}
		return "http://unix", &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", sock)
				},
			},
		}
	}
	return strings.TrimRight(apiURL, "/"), &http.Client{Timeout: timeout}
}

func httpDiscover(ctx context.Context, apiURL, projectID string) error {
	base, client := daemonHTTP(apiURL, 30*time.Second)
	u := base + "/api/v1/projects/" + projectID + "/mcp/tools"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if tok := daemonBearerToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("daemon call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var wrap struct {
		Tools []chat.Tool `json:"tools"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return fmt.Errorf("parse daemon response: %w", err)
	}
	if wrap.Tools == nil {
		wrap.Tools = []chat.Tool{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(wrap.Tools)
}

func httpCall(ctx context.Context, apiURL, projectID, qualifiedName, argsJSON string) error {
	// Per-call timeout distinct from the session-level ctx (see below).
	base, callClient := daemonHTTP(apiURL, 60*time.Second)
	u := base + "/api/v1/projects/" + projectID + "/mcp/tools/call"
	// Validate argsJSON BEFORE marshalling so a hallucinated /
	// truncated argument from the LLM produces a typed error
	// instead of an empty POST body. Pre-fix the marshal error
	// was discarded (`body, _ := json.Marshal(...)`) which sent
	// nil bytes when json.RawMessage was invalid — daemon would
	// 400 and the LLM would retry into a tight loop.
	if !json.Valid([]byte(argsJSON)) {
		preview := argsJSON
		if len(preview) > 200 {
			preview = preview[:200] + "…(truncated)"
		}
		return fmt.Errorf("argsJSON is not valid JSON (likely a hallucinated/truncated tool call): %s", preview)
	}
	body, err := json.Marshal(map[string]any{
		"name":      qualifiedName,
		"arguments": json.RawMessage(argsJSON),
	})
	if err != nil {
		return fmt.Errorf("build request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := daemonBearerToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	// Forward the agent container's task / execution origin so
	// the daemon can plumb them through to broker audit rows
	// (trading_orders.task_id / execution_id). Pre-2026-05-11
	// every trading_orders row had NULL task_id, blocking
	// task↔order correlation for investigations. These env vars
	// are stamped on the container by the executor; an
	// operator-driven CLI run won't have them set, which is
	// fine — the broker writes NULL columns.
	if v := os.Getenv("VORNIK_TASK_ID"); v != "" {
		req.Header.Set("X-Task-ID", v)
	}
	if v := os.Getenv("VORNIK_EXECUTION_ID"); v != "" {
		req.Header.Set("X-Execution-ID", v)
	}

	// callClient carries a per-call timeout distinct from the
	// session-level ctx (5min). A hung daemon-side handler would
	// otherwise pin every tool call to the full 5-minute budget — one
	// stalled call burns the entire session, cascading into executor
	// step timeouts and warm-container health releases.
	resp, err := callClient.Do(req)
	if err != nil {
		return fmt.Errorf("daemon call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var wrap struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &wrap); err != nil {
		return fmt.Errorf("parse daemon response: %w", err)
	}
	fmt.Print(wrap.Text)
	return nil
}
