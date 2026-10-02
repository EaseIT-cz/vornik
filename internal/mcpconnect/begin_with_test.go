package mcpconnect

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/mcpauth"
)

type countingTransport struct {
	n    atomic.Int32
	next http.RoundTripper
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.next.RoundTrip(r)
}

// Plan P4.4: an attempt records its origin and flow binding; Peek reads them
// without consuming the state (Complete still consumes it, once); and a
// project's discovery and exchange go through HTTPFor's client. Control:
// BeginWith, Peek and httpFor.
func TestBeginWith_OriginPeekAndClient(t *testing.T) {
	vendor := newOAuthServer(t, true, "read", "offline_access")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://vornik.example.com")
	guarded := &countingTransport{next: http.DefaultTransport}
	c.HTTPFor = func(projectID, _ string) *http.Client {
		if projectID == "hermes--work" {
			return &http.Client{Transport: guarded}
		}
		return nil
	}
	ref := ServerRef{ProjectID: "hermes--work", ServerName: "jira", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth, Scopes: []string{"read"}}}
	begun, err := c.BeginWith(context.Background(), ref, "device:d1", BeginOptions{Origin: "device:d1:apr_1:h", FlowBinding: "bind"})
	require.NoError(t, err)
	if guarded.n.Load() == 0 {
		t.Fatal("discovery did not use the project's client")
	}
	for i := 0; i < 2; i++ {
		origin, binding, ok := c.Peek(begun.State)
		if !ok || origin != "device:d1:apr_1:h" || binding != "bind" {
			t.Fatalf("peek %d: %q %q %v", i, origin, binding, ok)
		}
	}
	before := guarded.n.Load()
	_, err = c.Complete(context.Background(), begun.State, "code-1")
	require.NoError(t, err)
	if guarded.n.Load() == before {
		t.Fatal("the code exchange did not use the project's client")
	}
	if _, _, ok := c.Peek(begun.State); ok {
		t.Fatal("the state survived Complete")
	}
	// An operator attempt has no origin and uses the default client.
	op := ServerRef{ProjectID: "assistant", ServerName: "jira", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth, Scopes: []string{"read"}}}
	before = guarded.n.Load()
	b2, err := c.Begin(context.Background(), op, "alice")
	require.NoError(t, err)
	if origin, _, _ := c.Peek(b2.State); origin != "" || guarded.n.Load() != before {
		t.Fatalf("operator attempt: origin %q, guarded calls %d", origin, guarded.n.Load()-before)
	}
}
