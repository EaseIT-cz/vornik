package dialguard

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ErrCrossHostRedirect is SameHostRedirects' refusal of a redirect to another
// scheme, host or port, so a caller can classify it without matching text.
var ErrCrossHostRedirect = errors.New("dial guard: refusing a redirect to another host")

// ModelEndpointGuard is the guard of the clients the daemon builds for its
// configured model endpoints: the shared chat transport and the embedder
// (GitHub #505, 18-security-and-tenancy.md "Amendment 2026-10-28 (#505)").
// AllowPrivate keeps a model server on loopback or the LAN reachable and
// refuses link-local (the metadata service) and the other blocked forms.
//
// It is the one place managed hosting changes (SaaS MVP design 2.4.3, M1
// #560): there it returns AllowPrivate false together with the image's
// declared loopback servers, never AllowPrivate false alone.
//
// Managed (M1c D4): DialGuard{PrivateHosts: the vouched model hosts}, so a
// customer-typed (BYO) endpoint reaches public addresses only, and a vouched
// name may also reach a private one but never loopback, link-local or the
// metadata addresses. Read on every call (D0).
func ModelEndpointGuard() DialGuard {
	if p := currentManagedPolicy(); p != nil {
		return DialGuard{PrivateHosts: append([]string(nil), p.ModelHosts...)}
	}
	return DialGuard{AllowPrivate: true}
}

// maxRedirectRequests is Go's default bound (http.Client stops after 10
// requests), which a custom CheckRedirect replaces and must enforce itself.
const maxRedirectRequests = 10

// SameHostRedirects is an http.Client CheckRedirect that follows a redirect
// only to the same scheme, host and port as the original request, or an
// upgrade of the same host from http to https on the default ports, within
// Go's default bound of 10 requests. Any other redirect returns
// ErrCrossHostRedirect before the second host is dialled: Go strips only
// Authorization, Cookie and WWW-Authenticate on a cross-host hop, never a
// custom header, and a 307 or 308 replays the request body (GitHub #505, C2).
func SameHostRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirectRequests {
		return fmt.Errorf("stopped after %d requests", maxRedirectRequests)
	}
	if len(via) == 0 {
		return nil
	}
	from, to := via[0].URL, req.URL
	if sameOrigin(from, to) || httpsUpgrade(from, to) {
		return nil
	}
	return fmt.Errorf("%w: %s to %s", ErrCrossHostRedirect, from.Host, to.Host)
}

func sameOrigin(from, to *url.URL) bool {
	return strings.EqualFold(from.Scheme, to.Scheme) && strings.EqualFold(from.Hostname(), to.Hostname()) &&
		portOf(from.Scheme, from.Port()) == portOf(to.Scheme, to.Port())
}

// httpsUpgrade is http://h to https://h, both on their default ports: a LAN
// gateway moving its clients to TLS (review 727c F2). Never the reverse.
func httpsUpgrade(from, to *url.URL) bool {
	return strings.EqualFold(from.Scheme, "http") && strings.EqualFold(to.Scheme, "https") &&
		strings.EqualFold(from.Hostname(), to.Hostname()) &&
		portOf(from.Scheme, from.Port()) == "80" && portOf(to.Scheme, to.Port()) == "443"
}

// portOf is a URL's port with the scheme default filled in, so
// https://h and https://h:443 compare equal.
func portOf(scheme, port string) string {
	if port != "" {
		return port
	}
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
