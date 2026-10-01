package mcp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// CallToolOnce is the call path for an approved broker write (broker
// write-actions design §5.4, D7): exactly one attempt, and a caller can tell
// "never sent" from "may have been sent". Execute re-dials a dead connection
// and replays the call, which is right for a read and a possible double send
// for a write.

func TestCallToolOnce_DeadConnectionIsNotRedialledOrRetried(t *testing.T) {
	var dials atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		dials.Add(1)
		return healthyClient(t, cfg.Name, "sent twice"), nil
	})
	mgr := NewManager(zerolog.Nop())
	mgr.clients["proj"] = map[string]*Client{"gmail": deadStdioClient("gmail")}

	_, _, err := mgr.CallToolOnce(context.Background(), "proj", "mcp__gmail__send", `{"to":"a@b.c"}`)
	require.Error(t, err)
	require.Equal(t, int64(0), dials.Load(), "a write must never be re-dialled and replayed")
	require.False(t, errors.Is(err, ErrNotSent), "a dead connection may have taken the request: not provably unsent")
}

func TestCallToolOnce_NotSentCases(t *testing.T) {
	mgr := NewManager(zerolog.Nop())
	mgr.clients["proj"] = map[string]*Client{"restricted": restrictedClient(t, "restricted")}

	for name, tool := range map[string]string{
		"malformed name":       "gmail_send",
		"server not connected": "mcp__nope__send",
		"tool not allowed":     "mcp__restricted__do_thing",
	} {
		_, _, err := mgr.CallToolOnce(context.Background(), "proj", tool, `{}`)
		require.Errorf(t, err, name)
		require.Truef(t, errors.Is(err, ErrNotSent), "%s: %v must be ErrNotSent", name, err)
	}
}

func TestCallToolOnce_ReturnsTextAndToolErrorFlag(t *testing.T) {
	mgr := NewManager(zerolog.Nop())
	mgr.clients["proj"] = map[string]*Client{"ok": healthyClient(t, "ok", "message queued")}
	text, isErr, err := mgr.CallToolOnce(context.Background(), "proj", "mcp__ok__send", `{"to":"a@b.c"}`)
	require.NoError(t, err)
	require.False(t, isErr)
	require.Equal(t, "message queued", text)
}

// The existing agent-facing message must not change: agents read it.
func TestCallTool_NotAllowedMessageUnchanged(t *testing.T) {
	c := restrictedClient(t, "restricted")
	_, err := c.CallTool(context.Background(), "do_thing", nil)
	require.EqualError(t, err, `tool "do_thing" is not in allowed_tools for server "restricted"`)
}
