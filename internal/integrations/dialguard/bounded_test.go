package dialguard

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review e89f F6 (GitHub #433), moved with the function under #465: the bound
// at its edge. A body of exactly limit bytes is read whole; one byte more is
// refused, never truncated to a prefix.
func TestReadBoundedBody_Boundary_Issue433(t *testing.T) {
	const limit = 16
	for _, n := range []int{0, limit - 1, limit} {
		b, err := ReadBoundedBody(strings.NewReader(strings.Repeat("a", n)), limit)
		require.NoError(t, err, "n=%d", n)
		assert.Len(t, b, n)
	}
	_, err := ReadBoundedBody(strings.NewReader(strings.Repeat("a", limit+1)), limit)
	assert.EqualError(t, err, "response exceeds 16 bytes")
}

// GitHub #465: Allows is the guard's own match, so a caller reporting whether
// a dial skipped the IP check (the MCP probe's audit row) cannot disagree with
// the dial. An allowed host gets the plain dialer (no Control); any other host,
// including a case or trailing-dot variant, gets the guarded one.
func TestDialGuard_AllowsMatchesTheDialersBypass_Issue465(t *testing.T) {
	g := DialGuard{AllowedHosts: []string{"mcp.lan", "127.0.0.1"}}
	for _, host := range []string{"mcp.lan", "127.0.0.1", "MCP.lan", "mcp.lan.", "localhost", ""} {
		bypass := g.Dialer(host).Control == nil
		assert.Equal(t, bypass, g.Allows(host), "host %q", host)
	}
	assert.True(t, g.Allows("mcp.lan"))
	assert.False(t, g.Allows("MCP.lan"), "exact match, no folding (fails closed)")
}
