package ui

// /ui/credentials — the console page where an operator, or a managed
// instance's owner (tenant_admin) with no shell, adds, rotates and removes the
// credentials that un-namespaced secret://NAME references resolve to after the
// daemon's environment (GitHub #623). Values are write-only: never logged,
// never placed in an error, never rendered back; the page is served no-store.
// Every write and removal is audited (name, action, actor; never the value).
//
// The page is mounted at its exact path, outside adminRouter (whose guard
// admits only admin-class callers), and its one gate is
// api.OperatorSurfaceAccessFor with SurfaceOperatorCredentials: admin-class
// callers and a tenant_admin (the surface is granted) pass, a project-scoped
// caller is answered 404, everyone else 403.
//
// see LLD § https://docs.vornik.io, Amendment 2026-11-02 (#623)

import (
	"context"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// maxCredentialValueBytes caps a submitted value, as the approver credential
// page does.
const maxCredentialValueBytes = 16 << 10

// OperatorCredentials is the service side of the credentials page.
type OperatorCredentials interface {
	// Status lists every operator credential and every un-namespaced name the
	// configuration references, with where each value comes from. Never a
	// value.
	Status(ctx context.Context) (CredentialsView, error)
	Put(ctx context.Context, name string, value []byte, actor string) error
	Delete(ctx context.Context, name string) error
	// Refresh re-runs MCP registration so a server waiting for a credential
	// comes up: the daemon-level discovery registry and the projects'
	// connections.
	Refresh(ctx context.Context) (CredentialsRefresh, error)
}

// CredentialRow is one name on the page.
type CredentialRow struct {
	Name       string
	InStore    bool
	StoreSetAt time.Time
	StoreSetBy string
	InEnv      bool
	NeededBy   []string // servers whose auth references the name
}

// State is the row's label: which value a lookup uses.
func (c CredentialRow) State() string {
	switch {
	case c.InEnv && c.InStore:
		return "the environment value is used"
	case c.InEnv:
		return "from the environment"
	case c.InStore:
		return "set"
	default:
		return "missing"
	}
}

// CredentialsView is the page's list.
type CredentialsView struct {
	Rows []CredentialRow
	// KeyUnusable is set when the store key cannot open the store: the page
	// says so instead of showing an empty list.
	KeyUnusable bool
}

// CredentialsRefresh reports the re-registration after a change.
type CredentialsRefresh struct {
	// DaemonPresent lists the daemon-level servers now in the registry. Their
	// reachability probe runs in the background ("probe pending").
	DaemonPresent []string
	// ProjectDials lists "project/server: outcome" for the projects'
	// connections, dialled synchronously.
	ProjectDials []string
}

// WithOperatorCredentials wires the credentials page.
func WithOperatorCredentials(c OperatorCredentials) ServerOption {
	return func(s *Server) { s.operatorCredentials = c }
}

// AdminCredentialsData backs admin_credentials.html.
type AdminCredentialsData struct {
	adminCommonData
	View    CredentialsView
	Flash   string
	Error   string
	Refresh *CredentialsRefresh
}

// AdminCredentials serves GET (the list) and POST (save or remove).
func (s *Server) AdminCredentials(w http.ResponseWriter, r *http.Request) {
	switch api.OperatorSurfaceAccessFor(r, s.isAdminKeyForSurface, api.SurfaceOperatorCredentials) {
	case api.OperatorSurfaceAllowed:
	case api.OperatorSurfaceHidden:
		http.NotFound(w, r)
		return
	default:
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if s.operatorCredentials == nil {
		// The service always wires it; this guards a console built without it
		// (tests, a future edition), never a production path (code review F7).
		http.Error(w, "credentials are not available on this daemon", http.StatusServiceUnavailable)
		return
	}
	data := AdminCredentialsData{adminCommonData: adminCommonData{
		Title: "Credentials",
		// The second tab of the nav's "Keys & access" entry: the nav
		// highlights that entry on this page too (operator, 2026-11-02).
		CurrentPage: "admin-keys",
		IsAdmin:     api.IsAdminClassRequest(r, s.operatorCapability.IsAdminKey),
	}}
	if r.Method == http.MethodPost {
		s.adminCredentialsPost(w, r, &data)
	} else if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	view, err := s.operatorCredentials.Status(r.Context())
	if err != nil {
		data.Error = "the credential list could not be read"
		s.logger.Warn().Err(err).Msg("credentials page: status failed")
	}
	data.View = view
	s.render(w, "admin_credentials.html", data)
}

func (s *Server) adminCredentialsPost(w http.ResponseWriter, r *http.Request, data *AdminCredentialsData) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCredentialValueBytes+4096)
	if err := r.ParseForm(); err != nil {
		data.Error = "the form could not be read (a value is at most 16 KiB)"
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if !agentns.ValidSecretName(name) {
		data.Error = "names must be UPPER_SNAKE_CASE: A to Z, 0 to 9 and _, starting with a letter, at most 64 characters"
		return
	}
	actor := credentialsActor(r)
	switch r.PostFormValue("action") {
	case "save":
		value := r.PostFormValue("value")
		if value == "" {
			data.Error = "enter a value"
			return
		}
		if len(value) > maxCredentialValueBytes {
			data.Error = "a value is at most 16 KiB"
			return
		}
		if err := s.operatorCredentials.Put(r.Context(), name, []byte(value), actor); err != nil {
			// The store's errors name the credential, never the value.
			s.logger.Warn().Err(err).Str("credential", name).Msg("credentials page: save failed")
			data.Error = "the credential could not be stored"
			return
		}
		s.auditCredential(r, "credential.set", name, actor)
		data.Flash = name + " saved."
	case "remove":
		if err := s.operatorCredentials.Delete(r.Context(), name); err != nil {
			s.logger.Warn().Err(err).Str("credential", name).Msg("credentials page: remove failed")
			data.Error = "the credential could not be removed"
			return
		}
		s.auditCredential(r, "credential.remove", name, actor)
		data.Flash = name + " removed."
	default:
		data.Error = "unknown action"
		return
	}
	report, err := s.operatorCredentials.Refresh(r.Context())
	if err != nil {
		s.logger.Warn().Err(err).Msg("credentials page: MCP refresh failed")
		data.Error = "saved, but MCP servers could not be refreshed; a config reload will register them"
	}
	data.Refresh = &report
}

func (s *Server) auditCredential(r *http.Request, action, name, actor string) {
	if s.adminAuditRepo == nil {
		return
	}
	if err := s.adminAuditRepo.Insert(r.Context(), &persistence.AdminAuditEntry{
		Principal: actor,
		Source:    "ui",
		Action:    action,
		Target:    name,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	}); err != nil {
		s.logger.Warn().Err(err).Str("credential", name).Msg("credentials page: audit write failed")
	}
}

// credentialsActor names who acted: the session's user, else the admin
// principal (an admin key), as "console:<id>".
func credentialsActor(r *http.Request) string {
	if id := api.IdentityFromContext(r.Context()); id != nil {
		if uid, _ := id.Extra[auth.ExtraSessionUserID].(string); uid != "" {
			return "console:" + uid
		}
	}
	return "console:" + adminPrincipal(r)
}

// isAdminKeyForSurface is the admin-key checker the surface gate needs.
func (s *Server) isAdminKeyForSurface(key string) bool {
	return s.operatorCapability.IsAdminKey(key)
}
