package dispatcher

// GitHub #647 (dispatcher turn-context design §2): the 50 KiB MCP result cap
// cut by bytes and could split a multibyte character, sending invalid UTF-8 to
// the model. Found at: review (review-20261105-7883), observed at production
// when an MCP tool returns non-ASCII text over 50 KiB.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMCPResultCapIsRuneSafe_647(t *testing.T) {
	// "ž" is two bytes; an odd prefix puts byte mcpToolResultMaxBytes inside one.
	big := "x" + strings.Repeat("ž", mcpToolResultMaxBytes)
	te := &ToolExecutor{mcpManager: &stubMCP{out: big}}
	res := te.executeMCPTool(context.Background(), "p", "mcp__srv__tool", "{}")
	if !utf8.ValidString(res.Content) {
		t.Fatal("the capped MCP result must be valid UTF-8")
	}
	i := strings.Index(res.Content, "\n\n[truncated: kept first ")
	if i < 0 {
		t.Fatal("the cap must still apply")
	}
	// The content is wrapped in an untrusted-content label, so the kept
	// count is read from the footer, which reports the bytes kept.
	var kept, total int
	if _, err := fmt.Sscanf(res.Content[i:], "\n\n[truncated: kept first %d of %d bytes", &kept, &total); err != nil {
		t.Fatalf("footer: %v", err)
	}
	if kept > mcpToolResultMaxBytes || kept < mcpToolResultMaxBytes-3 || total != len(big) {
		t.Fatalf("kept %d of %d; want at most %d and at most 3 bytes under it (review 40c3 F2)", kept, total, mcpToolResultMaxBytes)
	}
}
