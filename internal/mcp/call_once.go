package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotSent marks an error raised before a tool call left the daemon: a
// malformed name, a server that is not connected, a tool outside
// allowed_tools, or the per-tool rate limit. Anything else returned by
// CallToolOnce means the request may have reached the server.
var ErrNotSent = errors.New("mcp: request not sent")

// CallToolOnce calls one tool exactly once, for an approved broker write
// (broker write-actions design §5.4, D7). Unlike Execute it never re-dials a
// dead connection and replays the call: for a write, the first attempt may
// already have taken effect. It reports the tool-level error flag separately
// instead of folding it into the text.
//
// Like Execute, it adds the daemon-supplied project_id: that is the
// daemon's own identity, not model content, and some servers require it.
// The client's credential-refresh replay on a vendor 401 still applies: a
// 401 means the vendor refused the request, so nothing was performed.
func (m *Manager) CallToolOnce(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, bool, error) {
	serverName, toolName, ok := parseQualifiedName(qualifiedName)
	if !ok {
		return "", false, fmt.Errorf("%w: invalid MCP tool name %q", ErrNotSent, qualifiedName)
	}
	argsJSON = injectDaemonSuppliedArgs(argsJSON, projectID)
	client := m.resolveClient(projectID, serverName)
	if client == nil && m.isPending(projectID, serverName) {
		// Dial on use, then re-resolve under a fresh read lock: nothing has
		// been sent, so the single call below keeps this method's contract. A
		// reload that removes the client in between fails with ErrNotSent,
		// which is still true (failed-connect recovery design, D3, F2).
		if m.dialPending(projectID, serverName, true) == nil {
			client = m.resolveClient(projectID, serverName)
		}
		if client == nil {
			return "", false, fmt.Errorf("%w: %s", ErrNotSent, m.pendingMessage(projectID, serverName))
		}
	}
	if client == nil {
		return "", false, fmt.Errorf("%w: MCP server %q not connected for project %q", ErrNotSent, serverName, projectID)
	}
	result, err := client.CallTool(ctx, toolName, json.RawMessage(argsJSON))
	if err != nil {
		var limited *ToolRateLimitError
		if errors.As(err, &limited) {
			return "", false, fmt.Errorf("%w: %v", ErrNotSent, err)
		}
		return "", false, fmt.Errorf("MCP tool %s: %w", qualifiedName, err)
	}
	return result.Text(), result.IsError, nil
}

// resolveClient reads one client under the read lock and releases it.
func (m *Manager) resolveClient(projectID, serverName string) *Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[projectID][serverName]
}
