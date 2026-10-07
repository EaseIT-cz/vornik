package approverdevice

import (
	"net/http"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// httpsRequiredMessage is the refusal body for plain http to a non-loopback
// host (design §9.2, amendment 2026-10-07, T12). The browser's same-origin
// check would otherwise fail with the wrong cause.
const httpsRequiredMessage = "Pairing and approving need HTTPS. Serve Vornik over HTTPS (or use it on this machine through localhost) to pair or approve. " +
	"If you already use HTTPS through a proxy, make sure it sets X-Forwarded-Proto: https and its address is listed in server.real_ip.trusted_proxies."

// requireSecureTransport refuses every request that is not HTTPS (per
// requestIsHTTPS, which honours X-Forwarded-Proto only from a trusted proxy
// since T15) and whose Host is
// not loopback, before any cookie or same-origin check.
func (s *Service) requireSecureTransport(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requestIsHTTPS(r) && !isLoopbackHost(r.Host) {
			if s.httpsRefused != nil {
				path := "approve"
				if strings.HasPrefix(r.URL.Path, "/ui/pair") {
					path = "pair"
				}
				s.httpsRefused(path)
			}
			s.renderStatus(w, http.StatusForbidden, "HTTPS required", httpsRequiredMessage)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WithHTTPSRefusalRecorder counts refusals by path (pair or approve).
func WithHTTPSRefusalRecorder(fn func(path string)) Option {
	return func(s *Service) { s.httpsRefused = fn }
}

// HTTPSRefusalMetrics holds vornik_approver_https_required_total{path}.
// Created with the service before the registry exists, attached later;
// nil-safe and a no-op until attached (same rule as HostActionMetrics).
type HTTPSRefusalMetrics struct {
	mu    sync.Mutex
	total *prometheus.CounterVec
}

// NewHTTPSRefusalMetrics returns an unattached holder.
func NewHTTPSRefusalMetrics() *HTTPSRefusalMetrics { return &HTTPSRefusalMetrics{} }

// Attach registers the counter once; later calls are no-ops.
func (m *HTTPSRefusalMetrics) Attach(registerer prometheus.Registerer) {
	if m == nil || registerer == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total != nil {
		return
	}
	m.total = promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik",
		Subsystem: "approver",
		Name:      "https_required_total",
		Help: "Approver-page requests refused because they came over plain http to a " +
			"non-loopback host, by page group: pair or approve.",
	}, []string{"path"})
}

// Record counts one refusal.
func (m *HTTPSRefusalMetrics) Record(path string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	c := m.total
	m.mu.Unlock()
	if c != nil {
		c.WithLabelValues(path).Inc()
	}
}
