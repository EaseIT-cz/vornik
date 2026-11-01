package api

import (
	"net/http"
	"path"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
)

// ListenerGate is the managed-hosting gate in front of the public listener
// and the agent socket (GitHub #560; 18-security-and-tenancy.md, "Amendment
// 2026-10-28 (#560, slice M1a)", A4). Before routing, it reads every value of
// every header a credential is read from (X-API-Key, and Authorization as
// Basic or Bearer), with the same per-form readers as extractAPIKey, and
// answers 401 when any of them is an admin-class key; and it answers 404 on
// /metrics, which in managed hosting is served on the operations listener
// only. No handler behind it sees an admin-class key from this listener,
// whichever resolver that handler uses.
func ListenerGate(isAdminKey func(string) bool, logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if path.Clean("/"+r.URL.Path) == "/metrics" {
				http.NotFound(w, r)
				return
			}
			if form, key := adminKeyPresented(r, isAdminKey); key != "" {
				logger.Warn().Str("form", form).Str("key_id", config.KeyHashID(key)).Str("path", r.URL.Path).
					Msg("listener gate: admin-class credential refused on the public listener")
				http.Error(w, "credential not accepted on this listener", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// adminKeyPresented returns the form and value of the first admin-class key
// presented anywhere on r, or "", "".
func adminKeyPresented(r *http.Request, isAdminKey func(string) bool) (form, key string) {
	if isAdminKey == nil {
		return "", ""
	}
	for _, v := range r.Header.Values("X-API-Key") {
		if k := apiKeyFromXAPIKey(v); k != "" && isAdminKey(k) {
			return "x-api-key", k
		}
	}
	for _, v := range r.Header.Values("Authorization") {
		if k, f := apiKeyFromAuthorization(v); k != "" && isAdminKey(k) {
			return f, k
		}
	}
	return "", ""
}
