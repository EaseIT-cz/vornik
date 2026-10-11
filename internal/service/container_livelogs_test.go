package service

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
)

// GitHub #92 (2026-10-08), live design Change C6: the daemon-log ring's tap
// sits beside stdout in the root writer and leaves stdout's bytes unchanged.
func TestContainerLiveLogs_TapLeavesStdoutBytesUnchanged(t *testing.T) {
	c := &Container{}
	var plain, tapped bytes.Buffer
	lines := [][]byte{
		[]byte(`{"level":"info","project":"p","message":"one"}` + "\n"),
		[]byte("not json at all\n"),
		[]byte(`{"level":"warn","task_id":"t","message":"two"}` + "\n"),
	}
	w := c.rootLogWriter(&tapped)
	for _, l := range lines {
		_, _ = plain.Write(l)
		n, err := w.Write(l)
		require.NoError(t, err)
		assert.Equal(t, len(l), n)
	}
	assert.Equal(t, plain.Bytes(), tapped.Bytes())
	assert.Len(t, c.daemonLogRing().Since(nil, true, time.Time{}, 10), 2)
}

// GitHub #92 (2026-10-08): the root logger feeds the ring from its first
// line, so every logger derived from it (before or after log forwarding is
// set up) is tapped.
func TestContainerLiveLogs_RootLoggerFeedsRing(t *testing.T) {
	c := &Container{Config: &config.Config{}}
	c.initLogger()
	derived := c.Logger.With().Str("component", "mcp").Logger()
	derived.Error().Str("project", "hermes--fin").Str("server", "mail").Int("http_status", 401).Msg("mcp: failed to connect")
	got := c.daemonLogRing().Since(map[string]bool{"hermes--fin": true}, false, time.Time{}, 10)
	require.Len(t, got, 1)
	assert.Equal(t, 401, got[0].HTTPStatus)
	assert.Equal(t, "mcp", got[0].Component)
}
