package api

// GitHub #792 (design 27 §C2, AGENTS.md §8): the client the daemon fetches
// GitHub's JWKS with has an explicit timeout, reads through a size cap, and
// dials through netguard. The handler tests inject the issuer's own client,
// so these are the only tests that build the production one.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOIDCJWKSClient792_TimeoutIsTenSeconds(t *testing.T) {
	assert.Equal(t, 10*time.Second, oidcJWKSClient().Timeout)
}

// netguard refuses a loopback destination; wrapping its transport in the size
// cap must keep that.
func TestOIDCJWKSClient792_DialsThroughNetguard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer srv.Close()
	resp, err := oidcJWKSClient().Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the JWKS client reached a loopback address")
	}
}

// netguard refuses the loopback httptest server, so the production client
// cannot fetch from it: the test checks the client's transport is the cap,
// then measures the cap over the test server's own transport.
func TestOIDCJWKSClient792_BodyIsCapped(t *testing.T) {
	_, ok := oidcJWKSClient().Transport.(cappedTransport)
	require.True(t, ok, "the production client reads through the cap")

	read := func(n int) ([]byte, error) {
		body := strings.Repeat("a", n)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		defer srv.Close()
		c := &http.Client{Transport: cappedTransport{base: srv.Client().Transport, max: oidcJWKSMaxBytes}}
		resp, err := c.Get(srv.URL)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		return io.ReadAll(resp.Body)
	}

	b, err := read(oidcJWKSMaxBytes)
	require.NoError(t, err, "a key set of exactly the cap reads whole")
	assert.Equal(t, oidcJWKSMaxBytes, len(b))

	// GitHub #902: an over-cap key set used to be cut silently, and every
	// token then read bad_token with a JSON parse error as its only cause.
	_, err = read(oidcJWKSMaxBytes + 1)
	require.Error(t, err, "a key set over the cap must fail the read, not be truncated")
	assert.Contains(t, err.Error(), "exceeds the 1048576-byte cap")
}
