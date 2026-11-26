package api

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"vornik.io/vornik/internal/netguard"
)

// oidcJWKSMaxBytes caps a JWKS response: go-oidc reads it whole.
const oidcJWKSMaxBytes = 1 << 20

// oidcJWKSClient is the HTTP client the GitHub Actions token backend fetches
// the issuer's keys with (GitHub #792, design 27 §C2): netguard-dialed, 10
// seconds, and a size-capped body.
func oidcJWKSClient() *http.Client {
	c := netguard.NewGuardedClient(10 * time.Second)
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = cappedTransport{base: base, max: oidcJWKSMaxBytes}
	return c
}

type cappedTransport struct {
	base http.RoundTripper
	max  int64
}

func (t cappedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{r: io.LimitReader(resp.Body, t.max+1), c: resp.Body, max: t.max}
	return resp, nil
}

// cappedBody reads at most max bytes. The byte past the cap is an error that
// names the cap, not a silent end of file: go-oidc would parse a truncated key
// set, fail, and the log would show only a parse error (GitHub #902, design 27
// §6). The read error is the cause OIDCTrustBackend.refuse logs.
type cappedBody struct {
	r    io.Reader
	c    io.Closer
	max  int64
	read int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += int64(n)
	if b.read > b.max {
		over := int(b.read - b.max) // at most 1: the reader holds cap+1 bytes
		b.read = b.max
		return n - over, fmt.Errorf("JWKS response exceeds the %d-byte cap", b.max)
	}
	return n, err
}

func (b *cappedBody) Close() error { return b.c.Close() }
