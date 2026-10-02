package approverdevice

import (
	"context"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/approval"
)

// Cookie names. The device cookie is the approver-device credential; the
// claim cookie is held only by a phone waiting for its enrollment approval.
const (
	CookieName      = "vornik_approver"
	ClaimCookieName = "vornik_approver_claim"
)

type ctxKey struct{}

type deviceCtx struct {
	device *Device
	token  string
}

// RequireDevice admits only requests carrying a valid device cookie. It reads
// no other credential (no X-API-Key, no Authorization, no vornik_session), so
// an admin key or an operator web session grants nothing here (design §9.2).
// GET without a device goes to the pairing page; anything else is 403. Every
// non-GET must also pass approval.CheckRequest (POST, same origin): with
// SameSite=Strict on the cookie that is the CSRF control of record for this
// surface (plan amendment 1).
func (s *Service) RequireDevice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(CookieName)
		var d *Device
		if err == nil {
			d, err = s.Authenticate(r.Context(), c.Value)
		}
		if err != nil || d == nil {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, "/ui/pair", http.StatusSeeOther)
				return
			}
			s.renderStatus(w, http.StatusForbidden, "Not an approver device", "This browser is not paired as an approver device. Pair it from the Vornik terminal with vornikctl pair-device.")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if err := approval.CheckRequest(r); err != nil {
				s.renderStatus(w, http.StatusForbidden, "Refused", "This request did not come from this page.")
				return
			}
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, deviceCtx{device: d, token: c.Value})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// FromRequest returns the authenticated device RequireDevice admitted.
func FromRequest(r *http.Request) (*Device, bool) {
	dc, ok := r.Context().Value(ctxKey{}).(deviceCtx)
	return dc.device, ok && dc.device != nil
}

func tokenFromRequest(r *http.Request) string {
	dc, _ := r.Context().Value(ctxKey{}).(deviceCtx)
	return dc.token
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// SetCookie issues the device cookie: HttpOnly, SameSite=Strict, scoped to
// /ui/, Secure on HTTPS, 90 days (the idle expiry is enforced server-side).
func SetCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: token, Path: "/ui/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r), MaxAge: int(IdleExpiry.Seconds())})
}

// ClearCookie removes the device cookie.
func ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/ui/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r), MaxAge: -1})
}

func setClaimCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{Name: ClaimCookieName, Value: token, Path: "/ui/pair", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r), MaxAge: int(ClaimTTL.Seconds())})
}

func clearClaimCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: ClaimCookieName, Value: "", Path: "/ui/pair", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r), MaxAge: -1})
}
