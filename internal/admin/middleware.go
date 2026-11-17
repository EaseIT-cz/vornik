// Package admin provides the daemon-level admin UI gate and audit
// helpers — see https://docs.vornik.io
//
// The gate is intentionally minimal in slice 1: it sits between the
// daemon's shared HTTP listener and the /ui/admin/* handler tree,
// applies the three-tier decision (disabled → 404; enabled +
// non-admin → 403; enabled + admin → 200), and stashes a "principal
// is admin" bit on the request context so downstream handlers can
// render the hidden-nav flag.
//
// Slice-2 follow-through (github-login phase 3): every request now
// carries an auth.Identity, and a browser session can resolve to
// role=admin. The gate consults the SessionRoleChecker closure
// (wired by the daemon to avoid an api ↔ admin import cycle, same as
// PrincipalExtractor) and admits a session-admin alongside the
// API-key admin allowlist. The external contract — same 404 / 403 /
// 200 matrix — is unchanged; session-admin is an additional way to
// satisfy the "admin caller" predicate, not a new response shape.
package admin

import (
	"context"
	"net/http"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
)

// contextKey is the package-local context key type.
type contextKey string

const (
	// principalKey carries the matched API-key string after a
	// successful admin auth. Downstream handlers read it via
	// PrincipalFromContext for audit-row attribution.
	principalKey contextKey = "admin_principal"
	// isAdminKey carries a bool — true when the request bearer is
	// in admin.allowed_keys. Used by the nav partial to decide
	// whether to render the "Admin" link.
	isAdminKey contextKey = "admin_is_admin"
)

// PrincipalExtractor pulls the authenticated API key from a request.
// The API package stashes it on the context as a private contextKey;
// rather than introduce a circular import (api ↔ admin) the daemon
// wires a small closure that reads api's private key and returns the
// string. nil-safe — the middleware treats a missing extractor as
// "no principal available", which falls into the unauthenticated
// branch.
type PrincipalExtractor func(r *http.Request) string

// AuthEnabledChecker reports whether API-key authentication is
// enabled for this request. When auth is disabled (single-
// operator dev / homelab mode) the admin gate disengages: every
// caller is treated as admin, since the concept of "admin key vs
// regular key" doesn't apply when there are no keys at all.
//
// Wired the same way PrincipalExtractor is: a small closure
// passed in by the daemon so the admin package doesn't import
// the api package. nil-safe — a nil checker is treated as "auth
// is enabled" (the safer fail-closed default for production
// deployments).
type AuthEnabledChecker func(r *http.Request) bool

// SessionRoleChecker reports whether the request authenticated via a
// browser session whose principal has role=admin. Wired the same way
// PrincipalExtractor is — a closure the daemon supplies reading api's
// private identity context key — so the admin package keeps no
// dependency on the api package. nil-safe: a nil checker means "no
// session-admin path available", and the gate falls back to the
// API-key admin allowlist alone.
type SessionRoleChecker func(r *http.Request) bool

// Middleware returns the admin gate. cfg carries the admin block;
// extract returns the authenticated API key (empty means "no auth
// present"); authEnabled reports whether API-key auth is on for
// this request; pathPrefix scopes the gate to URLs starting with
// that prefix (typically "/admin/" — the UI subtree handler
// already stripped the "/ui" mount prefix).
//
// Behaviour matrix (admin-ui-design.md §10, extended for the
// auth-disabled case):
//
//	api.auth_enabled=false               → pass through, IsAdmin=true
//	                                       (every caller trusted; the
//	                                       admin gate is meaningless
//	                                       when auth is off — see the
//	                                       2026-05-24 /ui/memory/operators
//	                                       bug fix)
//	admin.enabled=false                  → 404 (surface hidden entirely)
//	admin.enabled=true, no API key       → 401
//	admin.enabled=true, non-admin key    → 403 ("admin scope required")
//	admin.enabled=true, admin key        → pass through with IsAdmin=true
//
// Requests outside pathPrefix pass through unmodified — the gate is
// scoped, not global.
func Middleware(cfg config.AdminConfig, extract PrincipalExtractor, authEnabled AuthEnabledChecker, sessionAdmin SessionRoleChecker, pathPrefix string, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	var o middlewareOptions
	for _, opt := range opts {
		opt(&o)
	}
	isSessionAdmin := func(r *http.Request) bool {
		return sessionAdmin != nil && sessionAdmin(r)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Auth-disabled override applies to BOTH the gated and
			// non-gated branches. When auth is off there is no
			// notion of "non-admin caller" to distinguish — every
			// caller is implicitly trusted, so stamp IsAdmin=true
			// and let the request through.
			if authEnabled != nil && !authEnabled(r) {
				ctx := context.WithValue(r.Context(), isAdminKey, true)
				ctx = context.WithValue(ctx, principalKey, "auth-disabled")
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			if !pathInGate(r.URL.Path, pathPrefix) {
				next.ServeHTTP(w, stampNavAdmin(cfg, extract, isSessionAdmin, r))
				return
			}

			// Disabled: 404. NOT 403 — admin-ui-design.md §10 says
			// the surface must be invisible to probes when the
			// operator hasn't opted in.
			if !cfg.Enabled {
				http.NotFound(w, r)
				return
			}

			// Session-admin passes the gate directly (github-login
			// phase 3). Checked before the key path so a browser
			// admin without an api-key admin allowlist entry still
			// reaches the admin UI.
			if isSessionAdmin(r) {
				ctx := context.WithValue(r.Context(), principalKey, "session:admin")
				ctx = context.WithValue(ctx, isAdminKey, true)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// The instance owner's read of a console page (GitHub #787): marked
			// read-only, never admin. Checked after the session-admin path, so
			// an admin is never downgraded.
			if o.ownerRead != nil && o.ownerRead(r) {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ownerReadOnlyKey, true)))
				return
			}

			var key string
			if extract != nil {
				key = extract(r)
			}
			if key == "" {
				http.Error(w, "admin authentication required", http.StatusUnauthorized)
				return
			}
			if !cfg.IsAdminKey(key) {
				http.Error(w, "admin scope required", http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), principalKey, adminKeyPrincipal(key))
			ctx = context.WithValue(ctx, isAdminKey, true)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func adminKeyPrincipal(key string) string {
	if key == "" {
		return ""
	}
	sum := apikey.Hash(key)
	if len(sum) > 16 {
		sum = sum[:16]
	}
	return "api_key_sha256:" + sum
}

// pathInGate reports whether path falls under the admin gate. The
// gate matches both the bare prefix (e.g. "/admin") and the
// prefix-with-trailing-slash form ("/admin/sub/page"). Empty prefix
// is treated as "gate everything", which the call site uses for
// the global /ui/admin scope.
func pathInGate(path, prefix string) bool {
	if prefix == "" {
		return true
	}
	if path == prefix {
		return true
	}
	if len(path) >= len(prefix) && path[:len(prefix)] == prefix {
		// Require a trailing slash or end-of-string so "/administrate"
		// doesn't match "/admin".
		if len(path) == len(prefix) || path[len(prefix)] == '/' {
			return true
		}
	}
	return false
}

// IsAdminFromContext returns the IsAdmin flag the middleware
// stamped onto the request context. Defaults to false — handlers
// outside the admin tree never see a true value unless an admin
// is browsing the rest of the UI too.
func IsAdminFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(isAdminKey).(bool)
	return v
}

// PrincipalFromContext returns the admin principal string the gate
// stashed on the request context. Empty when no admin auth ran.
func PrincipalFromContext(ctx context.Context) string {
	v, _ := ctx.Value(principalKey).(string)
	return v
}

// ContextWithAdmin returns a context carrying the same admin flags the
// middleware stamps after a successful gate check. Intended for
// in-process callers and tests that invoke handlers directly.
func ContextWithAdmin(ctx context.Context, principal string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, isAdminKey, true)
	if principal != "" {
		ctx = context.WithValue(ctx, principalKey, principal)
	}
	return ctx
}

// ownerReadOnlyKey marks a request the gate admitted as the instance owner's
// read of the console (GitHub #787): never IsAdmin, so the router must
// re-check it with api.OwnerConsoleRead.
const ownerReadOnlyKey contextKey = "admin_owner_read_only"

// OwnerReadOnlyFromContext reports whether the gate admitted this request as
// the owner's read of the console.
func OwnerReadOnlyFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(ownerReadOnlyKey).(bool)
	return v
}

// MiddlewareOption configures Middleware.
type MiddlewareOption func(*middlewareOptions)

type middlewareOptions struct{ ownerRead func(*http.Request) bool }

// WithOwnerRead lets the gate admit a request ownerRead accepts (the instance
// owner's read of a console page, GitHub #787) as read-only: it is marked
// OwnerReadOnly and never IsAdmin.
func WithOwnerRead(ownerRead func(*http.Request) bool) MiddlewareOption {
	return func(o *middlewareOptions) { o.ownerRead = ownerRead }
}

// stampNavAdmin stamps IsAdmin on a request outside the admin subtree, so the
// shared nav partial knows whether to render the Admin link on every page
// (not just inside the admin subtree). Without this an admin visiting /ui/
// would never see the link. A session-admin sees it too, stamped after the
// key path so the key principal wins when both are somehow present. Split out
// of Middleware (GitHub #787: the owner-read branch took it over the
// complexity limit).
func stampNavAdmin(cfg config.AdminConfig, extract PrincipalExtractor, isSessionAdmin func(*http.Request) bool, r *http.Request) *http.Request {
	if !cfg.Enabled {
		return r
	}
	if extract != nil {
		if key := extract(r); cfg.IsAdminKey(key) {
			ctx := context.WithValue(r.Context(), isAdminKey, true)
			ctx = context.WithValue(ctx, principalKey, adminKeyPrincipal(key))
			r = r.WithContext(ctx)
		}
	}
	if !IsAdminFromContext(r.Context()) && isSessionAdmin(r) {
		ctx := context.WithValue(r.Context(), isAdminKey, true)
		ctx = context.WithValue(ctx, principalKey, "session:admin")
		r = r.WithContext(ctx)
	}
	return r
}
