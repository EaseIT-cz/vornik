package api

import (
	"net/http"

	"vornik.io/vornik/internal/config"
)

// companionKeyRateLimited applies the default per-key limit to a companion
// key whose row has no limit of its own (GitHub #847, design 26 §4.2), and
// reports whether it wrote a 429. As the A2A caller default does, it checks a
// defaulted copy and never writes the row.
//
// Skipped when:
//   - the bearer is not an active companion key (AuthMiddleware already
//     answered an unknown key with 401; a non-companion key is refused
//     where it is used);
//   - the row has a limit of its own (AuthMiddleware enforced it, so the key
//     is checked once);
//   - api.rate_limit.companion_key.off is set.
func (s *Server) companionKeyRateLimited(w http.ResponseWriter, r *http.Request) bool {
	if s.apiKeyLimiter == nil {
		return false
	}
	key, err := s.resolveCompanionKey(r)
	if err != nil || key == nil || (key.RateLimitRPS != nil && key.RateLimitBurst != nil) {
		return false
	}
	var cl config.CompanionKeyRateLimit
	if s.config != nil {
		cl = s.config.API.RateLimit.CompanionKey
	}
	rps, burst, on := cl.Effective()
	if !on {
		return false
	}
	limited := *key
	limited.RateLimitRPS, limited.RateLimitBurst = &rps, &burst
	return enforceKeyRateLimit(w, s.apiKeyLimiter, s.rateLimitMetrics, &limited)
}
