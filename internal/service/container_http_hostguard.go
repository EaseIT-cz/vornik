package service

import (
	"net/http"
	"net/url"
	"path"
	"sync/atomic"
	"time"

	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/httpx/hostguard"
)

// hostRefusalLogEvery bounds the refusal log: at most one Warn line per
// interval, so a rebinding page cannot flood the log.
const hostRefusalLogEvery = time.Minute

// hostRefusalLogMax truncates the refused name in the log line.
const hostRefusalLogMax = 100

// csrfOffLogSiteMax truncates the Sec-Fetch-Site value in the refusal log.
const csrfOffLogSiteMax = 20

// csrfOffRefusalText is the fixed 403 body of the auth-off CSRF gate
// (GitHub #494). It echoes nothing from the request.
const csrfOffRefusalText = "CSRF: cross-site state-changing request refused. api.auth_enabled is false, " +
	"so the daemon accepts a state change only from its own pages or from a client that " +
	"sends no browser origin. Enable auth, or send the request from the daemon's own origin.\n"

// tcpRootHandler wraps the TCP listener's root handler (realIPMW(mux)) for the
// http.Server initHTTPServer builds.
//
// With api.auth_enabled false (which also covers api.auth_dry_run: config
// validation refuses it together with auth on) it adds the Host allowlist,
// the DNS-rebinding guard of GitHub #465, and inside it the auth-off CSRF gate
// of GitHub #494. With auth on, only an unauthenticated /metrics
// (metrics.require_admin false) is Host-checked (GitHub #494); the auth stack
// decides everything else. The unix socket server is never wrapped: its peer
// is a local process, not a browser.
// see LLD § https://docs.vornik.io "Amendment 2026-10-21 (#465)"
// see LLD § https://docs.vornik.io "Amendment 2026-10-21 (#494, #448)"
func (c *Container) tcpRootHandler(inner http.Handler) (http.Handler, error) {
	allow, err := c.hostAllowlist()
	if err != nil {
		return nil, err
	}
	guard := hostguard.Middleware(allow, c.hostRefusalLogger("tcp"))
	if c.Config.API.AuthEnabled {
		if c.Config.Metrics.RequireAdmin {
			return inner, nil
		}
		// Invariant (code review ca09): with auth on, every route other
		// than /metrics is auth-gated or signature-gated (webhooks), or is a
		// public endpoint that carries no tenant data (health, A2A cards,
		// login). A new unauthenticated route with data needs this check.
		guarded := guard(inner)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if path.Clean("/"+r.URL.Path) == "/metrics" {
				guarded.ServeHTTP(w, r)
				return
			}
			inner.ServeHTTP(w, r)
		}), nil
	}
	c.Logger.Info().
		Strs("accepted_hosts", allow.Names()).
		Msg("api.auth_enabled is false: the TCP listener answers only IP literals, localhost and these names (DNS-rebinding guard), and refuses cross-site state changes (CSRF guard); add names to server.accepted_hosts")
	return guard(c.csrfOffMiddleware(inner)), nil
}

// guardMetricsListener puts the Host allowlist in front of the dedicated
// metrics listener, which answers without a credential in every auth mode
// (GitHub #494). Called before the listener starts; an error stops the start.
// It applies whatever metrics.require_admin says: require_admin gates only the
// main port's /metrics, and this listener has no credential check at all.
func (c *Container) guardMetricsListener() error {
	if c.Observability == nil || c.Observability.Metrics == nil {
		return nil
	}
	allow, err := c.hostAllowlist()
	if err != nil {
		return err
	}
	c.Observability.Metrics.SetHandlerWrapper(hostguard.Middleware(allow, c.hostRefusalLogger("metrics")))
	return nil
}

// hostAllowlist is the one list of Host names the daemon's unauthenticated
// surfaces answer (GitHub #465, #494): server.accepted_hosts, validated (a
// malformed entry is a startup error naming it), plus the names the operator
// already declared: server.public_base_url, and the public origin
// (PublicOrigin: public_base_url, else auth.external_base_url; the field is
// never read directly). A declared host that is not a DNS name is skipped (an
// IP literal is accepted anyway).
func (c *Container) hostAllowlist() (hostguard.Allowlist, error) {
	names := append([]string(nil), c.Config.Server.AcceptedHosts...)
	if _, err := hostguard.NewAllowlist(names); err != nil {
		return hostguard.Allowlist{}, err
	}
	for _, origin := range []string{c.Config.Server.PublicBaseURL, c.Config.PublicOrigin()} {
		if h := originHost(origin); h != "" {
			if _, err := hostguard.NewAllowlist([]string{h}); err == nil {
				names = append(names, h)
			}
		}
	}
	return hostguard.NewAllowlist(names)
}

// hostRefusalLogger is the rate-limited log of a Host refusal on one listener.
func (c *Container) hostRefusalLogger(listener string) func(string) {
	var lastLog atomic.Int64
	return func(host string) {
		if !rateLimitOK(&lastLog) {
			return
		}
		if len(host) > hostRefusalLogMax {
			host = host[:hostRefusalLogMax]
		}
		c.Logger.Warn().
			Str("listener", listener).
			Str("host", host).
			Msg("refused a request whose Host is not one of this daemon's names (unauthenticated surface; DNS-rebinding guard); list the name in server.accepted_hosts if it is yours")
	}
}

// csrfOffMiddleware is the CSRF gate of a daemon with auth off (GitHub #494):
// it refuses a request approval.CrossSiteWrite names, a state change a
// browser sent on behalf of another site. A request with no browser origin
// signal (a CLI, an agent, a webhook sender) is served.
func (c *Container) csrfOffMiddleware(next http.Handler) http.Handler {
	var lastLog atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !approval.CrossSiteWrite(r) {
			next.ServeHTTP(w, r)
			return
		}
		if rateLimitOK(&lastLog) {
			p := path.Clean("/" + r.URL.Path)
			if len(p) > hostRefusalLogMax {
				p = p[:hostRefusalLogMax]
			}
			// The Origin is not logged: the sender chooses it. Sec-Fetch-Site
			// is a browser-set header; a non-browser can send anything, so it
			// is truncated.
			site := r.Header.Get("Sec-Fetch-Site")
			if len(site) > csrfOffLogSiteMax {
				site = site[:csrfOffLogSiteMax]
			}
			c.Logger.Warn().
				Str("method", r.Method).
				Str("path", p).
				Str("sec_fetch_site", site).
				Msg("refused a cross-site state-changing request (api.auth_enabled is false; CSRF guard)")
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(csrfOffRefusalText))
	})
}

// rateLimitOK reports whether a rate-limited log line may be written now (at
// most one per hostRefusalLogEvery), recording the time when it may.
func rateLimitOK(last *atomic.Int64) bool {
	now := time.Now().UnixNano()
	prev := last.Load()
	return now-prev >= int64(hostRefusalLogEvery) && last.CompareAndSwap(prev, now)
}

// originHost returns the host of a scheme://host[:port] origin, or "".
func originHost(origin string) string {
	if origin == "" {
		return ""
	}
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
