package api

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"vornik.io/vornik/internal/integrations/dialguard"
)

// gatewayCheckTimeout bounds the doctor's gateway probe.
const gatewayCheckTimeout = 3 * time.Second

// checkGatewayHealthy pings the local API gateway. SKIPPED when unconfigured so
// a daemon without the gateway isn't marked unhealthy (design §9 doctor
// "gateway reachable"). Any HTTP response counts as reachable; a request-build
// error or transport error is ERROR.
func (h *DoctorHandlers) checkGatewayHealthy(ctx context.Context, _ bool) DoctorCheck {
	const name = "gateway_healthy"
	if h == nil || h.gatewayURL == "" {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "API gateway not configured; skipping"}
	}
	// review B1: never GET a non-loopback host. The gateway is a
	// loopback-only local sidecar; a non-loopback gatewayURL is
	// misconfiguration, and probing it would make the doctor itself perform
	// an SSRF. Reject on the parsed host BEFORE dialing (config.Validate
	// enforces the same rule at startup; this is defense-in-depth).
	if u, err := url.Parse(h.gatewayURL); err != nil || !dialguard.IsLoopbackHost(u.Hostname()) {
		return DoctorCheck{Name: name, Status: "ERROR", Message: "gateway address must be loopback"}
	}
	ctx, cancel := context.WithTimeout(ctx, gatewayCheckTimeout)
	defer cancel()
	// Hit the gateway base URL and treat any HTTP response as "reachable".
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.gatewayURL, nil)
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: "gateway URL invalid: " + err.Error()}
	}
	// GitHub #494 (integrations-hub-design "Amendment 2026-10-21 (#494)"):
	// http.DefaultClient followed a gateway's redirect to any address. The
	// shared guard dials only the URL's own loopback host (which must resolve
	// to loopback), and a 3xx comes back as the response: reachable, with no
	// second request.
	client := dialguard.LoopbackURLGuard(h.gatewayURL, nil).NoRedirectHTTPClient(gatewayCheckTimeout)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: "gateway unreachable: " + err.Error()}
	}
	_ = resp.Body.Close()
	return DoctorCheck{Name: name, Status: "OK", Message: "gateway reachable"}
}

// SetGatewayURL wires the gateway endpoint; empty leaves the check SKIPPED.
func (h *DoctorHandlers) SetGatewayURL(u string) {
	if h != nil {
		h.gatewayURL = u
	}
}
