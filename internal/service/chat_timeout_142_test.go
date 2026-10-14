package service

// GitHub #142 item 1 (2026-10-14, outcome-quality-design §S8 P2/F2): on the
// three single-provider paths (http, claude-cli, codex-cli) a chat.timeout of
// "0s" was passed through, and every provider applies its per-call bound only
// when it is positive, so "0s" removed the inner timeout entirely. The router
// path and Bedrock already ignored a non-positive value.

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestChatCallTimeout_ANonPositiveValueKeepsTheProviderDefault(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  time.Duration
		apply bool
	}{
		{"", 0, false},
		{"not-a-duration", 0, false},
		{"0s", 0, false},
		{"-5s", 0, false},
		{"600s", 600 * time.Second, true},
	} {
		got, apply := chatCallTimeout(tc.raw, zerolog.Nop())
		if apply != tc.apply || got != tc.want {
			t.Errorf("chatCallTimeout(%q) = (%v, %v), want (%v, %v): a non-positive chat.timeout must not remove the per-call bound",
				tc.raw, got, apply, tc.want, tc.apply)
		}
	}
}
