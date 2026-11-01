package ui

// The managed instance's companion page (GitHub #587,
// operations-api-contract-design.md §7.2.3, §7.2.3a). The owner connects
// their coding assistant: the page shows the install steps, which carry no
// key, and its script calls the existing companion routes to create, list
// and revoke keys, under the console's session and CSRF rules. So the page
// mints nothing itself and its HTML never holds a key; the key exists only
// in the browser's memory, written with textContent, until the page is left.

import (
	"net/http"
	"strings"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/listener"
)

// companionProjectID is the project the Engineering template seeds (§7.2.1).
const companionProjectID = "companion"

// CompanionPageData backs companion.html.
type CompanionPageData struct {
	adminCommonData
	// HasProject: the Engineering template was seeded.
	HasProject bool
	// VornikURL is the instance's address the companion connects to.
	VornikURL string
}

// ownerOnPublicListener reports whether r is the managed instance's owner,
// signed in, on the public listener: the rule the owner's pages share.
func ownerOnPublicListener(r *http.Request) bool {
	return listener.FromContext(r.Context()) == listener.Public &&
		api.SessionRoleFromContext(r.Context()) == auth.RoleTenantAdmin
}

// CompanionPage serves GET /ui/companion.
func (s *Server) CompanionPage(w http.ResponseWriter, r *http.Request) {
	if !s.managedHosting {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "the companion page is read only", http.StatusMethodNotAllowed)
		return
	}
	if !ownerOnPublicListener(r) {
		http.Error(w, "the companion page is the instance owner's, signed in", http.StatusForbidden)
		return
	}
	base := s.webUIBaseURL
	if base == "" {
		base = "https://" + r.Host
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "companion.html", CompanionPageData{
		adminCommonData: adminCommonData{Title: "Connect your companion", CurrentPage: "companion"},
		HasProject:      s.projectReg != nil && s.projectReg.GetProject(companionProjectID) != nil,
		VornikURL:       strings.TrimRight(base, "/"),
	})
}
