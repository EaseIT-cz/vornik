package report

// GitHub #439, #440 (2026-10-21): ScrubLines is the one scrub of a text the
// held log's scrubbed view, the explain renderer, the post-mortem explainer
// and the companion task_logs tool share.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScrubLines_WithholdsTheSecretLineOnly(t *testing.T) {
	scrub, err := NewLineScrubber("")
	require.NoError(t, err)
	tok := "ghp_" + strings.Repeat("Ab3dE6gH9k", 4)[:36]
	out, n := ScrubLines("step failed\nTOKEN="+tok+"\nexit status 1", scrub)
	assert.Equal(t, 1, n)
	assert.Equal(t, "step failed\n"+WithheldLine+"\nexit status 1", out)
}

func TestScrubLines_NilScrubberWithholdsEverything(t *testing.T) {
	out, n := ScrubLines("a\n\nb", nil)
	assert.Equal(t, 2, n)
	assert.Equal(t, WithheldLine+"\n\n"+WithheldLine, out)
	out, n = ScrubLines("", nil)
	assert.Equal(t, "", out)
	assert.Zero(t, n)
}
