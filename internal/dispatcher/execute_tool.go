package dispatcher

import (
	"context"

	"vornik.io/vornik/internal/chat"
)

// ExecuteTool runs one dispatcher tool call exactly as a chat turn would —
// through ToolExecutor.Execute, with its audit write — but without an LLM,
// a session or a project scope. It exists for end-to-end tests that must
// prove a tool works against real dependencies (test/e2e drives
// render_document with the real agent image through it). fs receives any
// file the tool delivers.
//
// It BYPASSES session and project scope: no project is resolved, so a
// project-scoped tool sees none, and nothing checks the caller may use the
// tool. It must therefore never be wired to a request path — only a test or a
// trusted in-process caller may reach it (S4 review F7).
func (a *Agent) ExecuteTool(ctx context.Context, name, argsJSON string, fs FileSender) ToolResult {
	if a == nil || a.toolExecutor == nil {
		return ToolResult{Content: "dispatcher not configured"}
	}
	tc := chat.ToolCall{Type: "function", Function: chat.FunctionCall{Name: name, Arguments: argsJSON}}
	return a.toolExecutor.Execute(ctx, tc, "", nil, 0, "", fs)
}
