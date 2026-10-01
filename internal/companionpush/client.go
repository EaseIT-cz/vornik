package companionpush

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// attemptTimeout bounds one POST (design §7).
const attemptTimeout = 10 * time.Second

type allowedKey struct{}

// client posts one push through a connect-time reach guard. The project's
// allowlist travels in the request context to the dialer, and keep-alives
// are off, so a connection opened under one project's allowlist is never
// reused for another project's push (design §7a).
type client struct {
	http  *http.Client
	reach func(netip.Addr, []netip.Prefix) bool
}

func newClient() *client {
	c := &client{reach: reachAllowed}
	dialer := &net.Dialer{Timeout: attemptTimeout}
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			allowed, _ := ctx.Value(allowedKey{}).([]netip.Prefix)
			d := *dialer
			d.Control = func(_, addr string, _ syscall.RawConn) error {
				ap, err := netip.ParseAddrPort(addr)
				if err != nil {
					return fmt.Errorf("companion push: refusing unparsable address %q", addr)
				}
				if !c.reach(ap.Addr(), allowed) {
					return fmt.Errorf("companion push: %w: refusing to connect to %s (not public, and not in the project's allowed_cidrs)", errRefused, ap.Addr())
				}
				return nil
			}
			return d.DialContext(ctx, network, address)
		},
	}
	c.http = &http.Client{
		Timeout:   attemptTimeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("companion push: redirects are not allowed")
		},
	}
	return c
}

// errRefused marks a push the reach guard refused (not worth retrying).
var errRefused = errors.New("refused by the reach guard")

// post sends one push. A non-2xx answer is an error.
func (c *client) post(ctx context.Context, url, token string, body []byte, allowed []netip.Prefix) error {
	ctx = context.WithValue(ctx, allowedKey{}, allowed)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "vornik-companion-push/1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("companion push: receiver answered HTTP %d", resp.StatusCode)
	}
	return nil
}
