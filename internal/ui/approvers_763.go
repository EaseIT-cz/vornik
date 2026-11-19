package ui

// GitHub #763 (agent-administered design part 09, amendment #763): the
// console lists, pairs, revokes and removes approver devices. Only an
// owner-class browser session may: an API key of any class is refused, so an
// assistant holding the admin key `vornikctl connect` mints cannot pair
// here. A tenant_admin pairs only a further device (the first comes out of
// band on managed hosting) and may not revoke the last live one. The service
// audits each change (StartPairingBy, RevokeBy, Remove).

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// ApproverDevices is what the console needs of approverdevice.Service.
type ApproverDevices interface {
	StartPairingBy(ctx context.Context, label, principal, source string) (string, time.Time, error)
	DeviceExists(ctx context.Context) (bool, error)
	ListDevices(ctx context.Context) ([]persistence.ApproverDeviceRow, error)
	RevokeBy(ctx context.Context, id, principal, source string) error
	RevokeKeepingOne(ctx context.Context, id, principal, source string) error
	Remove(ctx context.Context, id, principal, source string) error
	PushConfigured() bool
}

// WithApproverDevices wires the approver-device service for the console's
// Approvers page (#763).
func WithApproverDevices(d ApproverDevices) ServerOption {
	return func(s *Server) { s.approverDevices = d }
}

// ApproversData is the Approvers page.
type ApproversData struct {
	adminCommonData
	Devices  []persistence.ApproverDeviceRow
	Live     int
	Code     string // shown once, on the mint response only
	Expires  time.Time
	PairURL  string
	HTTPWarn bool
	NoAlert  bool
	Notice   string
}

// approverRole admits an owner-class browser session and answers its role.
// Anything else (an API key of any class, auth off, a user session) is
// refused with 403 and "" is returned.
func (s *Server) approverRole(w http.ResponseWriter, r *http.Request) string {
	id := api.IdentityFromContext(r.Context())
	if id == nil || id.Backend != "session" {
		http.Error(w, "approver devices are managed from a signed-in browser session; with auth off, use vornikctl pair-device", http.StatusForbidden)
		return ""
	}
	role := api.SessionRoleFromContext(r.Context())
	if role != auth.RoleAdmin && role != auth.RoleTenantAdmin {
		http.Error(w, "the instance owner manages approver devices", http.StatusForbidden)
		return ""
	}
	return role
}

// Approvers serves /settings/approvers and its POST actions.
func (s *Server) Approvers(w http.ResponseWriter, r *http.Request) {
	role := s.approverRole(w, r)
	if role == "" {
		return
	}
	if s.approverDevices == nil {
		http.Error(w, "approver devices are not available on this instance", http.StatusServiceUnavailable)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/settings/approvers"), "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		s.renderApprovers(w, r, ApproversData{Notice: r.URL.Query().Get("notice")})
	case rest == "pair" && r.Method == http.MethodPost:
		s.pairApprover(w, r, role)
	case strings.HasSuffix(rest, "/revoke") && r.Method == http.MethodPost:
		s.revokeApprover(w, r, role, strings.TrimSuffix(rest, "/revoke"))
	case strings.HasSuffix(rest, "/remove") && r.Method == http.MethodPost:
		s.removeApprover(w, r, strings.TrimSuffix(rest, "/remove"))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) renderApprovers(w http.ResponseWriter, r *http.Request, data ApproversData) {
	data.adminCommonData = adminCommonData{Title: "Approver devices", CurrentPage: "approvers"} // its own Manage entry (#833, design §21)
	devs, err := s.approverDevices.ListDevices(r.Context())
	if err != nil {
		http.Error(w, "the device list could not be read", http.StatusInternalServerError)
		return
	}
	data.Devices = devs
	for _, d := range devs {
		if d.RevokedAt == nil {
			data.Live++
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "approvers.html", data)
}

func (s *Server) pairApprover(w http.ResponseWriter, r *http.Request, role string) {
	exists, err := s.approverDevices.DeviceExists(r.Context())
	if err != nil {
		http.Error(w, "the device list could not be read", http.StatusInternalServerError)
		return
	}
	if role == auth.RoleTenantAdmin && !exists {
		http.Error(w, "The first approver device of a managed instance is paired with a code delivered out of band (at sign-up, or by email to the owner's verified address).", http.StatusForbidden)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		label = "Phone paired " + time.Now().Format("2006-01-02 15:04")
	}
	code, expires, err := s.approverDevices.StartPairingBy(r.Context(), label, adminPrincipal(r), "ui")
	switch {
	case errors.Is(err, approverdevice.ErrBadLabel):
		s.renderApprovers(w, r, ApproversData{Notice: "The label must be 1 to 40 characters."})
		return
	case err != nil:
		s.logger.Error().Err(err).Msg("approvers: a pairing code could not be issued")
		http.Error(w, "a pairing code could not be issued", http.StatusInternalServerError)
		return
	}
	origin := s.approverOrigin(r)
	if len(code) == 8 {
		code = code[:4] + "-" + code[4:]
	}
	s.renderApprovers(w, r, ApproversData{Code: code, Expires: expires, PairURL: origin + "/ui/pair",
		HTTPWarn: plainHTTPNonLoopbackOrigin(origin), NoAlert: !s.approverDevices.PushConfigured()})
}

// approverOrigin is the service's configured public origin, else this
// console's own.
func (s *Server) approverOrigin(r *http.Request) string {
	if o, ok := s.approverDevices.(interface{ Origin() string }); ok && o.Origin() != "" {
		return o.Origin()
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func plainHTTPNonLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname() // no brackets, no port: "::1" for "[::1]:8080"
	return host != "localhost" && host != "127.0.0.1" && host != "::1"
}

func (s *Server) revokeApprover(w http.ResponseWriter, r *http.Request, role, id string) {
	revoke := s.approverDevices.RevokeBy
	if role == auth.RoleTenantAdmin {
		// The last live device: a tenant_admin may not (the service checks
		// and revokes under one lock).
		revoke = s.approverDevices.RevokeKeepingOne
	}
	notice := "Revoked"
	switch err := revoke(r.Context(), id, adminPrincipal(r), "ui"); {
	case errors.Is(err, persistence.ErrNotFound):
		http.Error(w, "no such approver device", http.StatusNotFound)
		return
	case errors.Is(err, approverdevice.ErrAlreadyRevoked):
		http.Error(w, "that device is already revoked", http.StatusConflict)
		return
	case errors.Is(err, approverdevice.ErrLastDevice):
		http.Error(w, "This is the last approver device; revoke it from the phone itself.", http.StatusConflict)
		return
	case errors.Is(err, approverdevice.ErrNoAudit):
		s.logger.Error().Err(err).Str("device", id).Msg("approvers: a device was revoked but the admin audit could not record it")
		notice = "Revoked, but the admin audit could not record it"
	case err != nil:
		http.Error(w, "the device could not be revoked", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/settings/approvers?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

func (s *Server) removeApprover(w http.ResponseWriter, r *http.Request, id string) {
	switch err := s.approverDevices.Remove(r.Context(), id, adminPrincipal(r), "ui"); {
	case errors.Is(err, persistence.ErrNotFound):
		http.Error(w, "no such approver device", http.StatusNotFound)
		return
	case errors.Is(err, persistence.ErrApproverDeviceLive):
		http.Error(w, "revoke the device before removing it", http.StatusConflict)
		return
	case err != nil:
		http.Error(w, "the device could not be removed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/settings/approvers?notice=Removed", http.StatusSeeOther)
}
