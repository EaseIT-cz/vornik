package ui

import (
	"net/http"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
)

// manageCard is one card of the Manage overview (ui-design-refresh-design.md
// §17.2 M1, #759): what a Manage page is for, and a link to it.
type manageCard struct {
	Key, Label, Purpose, Href string
}

// managePurpose is each Manage destination's one line, in plain words.
var managePurpose = map[string]string{
	"projects":     "Your assistants: what each does, its settings and its keys.",
	"integrations": "The channels and services your assistants can use.",
	"admin-keys":   "API keys and the credentials Vornik keeps for you.",
	"admin-agents": "The coding agents connected to this instance.",
	"admin-skills": "Know-how your assistants apply, and the drafts waiting for approval.",
	"reminders":    "Reminders and recurring jobs you have scheduled.",
	"insights":     "What each tool costs against its budget.",
	"trading":      "The trading assistant's settings and positions.",
	"my-account":   "Your profile, linked chat accounts and sessions.",
}

// manageCards decides on the server which Manage cards the viewer sees
// (design §17.2 M1). It reads the Manage entries of the edition-filtered
// model the rail renders, with one trading predicate; that model is built in
// memory, so the overview costs no query. An admin (an admin key or session,
// or auth off) sees every card. The owner (tenant_admin) sees the non-admin
// cards, and each OwnerHref entry at that address. Everyone else sees the
// non-admin cards. A card is a view, not a gate: each destination keeps its
// own server gate.
func (s *Server) manageCards(r *http.Request, tradingEnabled func() bool) []manageCard {
	ctx := r.Context()
	isAdmin := admin.IsAdminFromContext(ctx) || !api.IsAuthEnabledFromContext(ctx)
	isOwner := api.SessionRoleFromContext(ctx) == auth.RoleTenantAdmin
	var cards []manageCard
	for _, a := range navModelFunc(tradingEnabled)() {
		if a.Key != "manage" {
			continue
		}
		for _, d := range a.Dests {
			if d.Key == "manage-overview" {
				continue
			}
			href := d.Href
			if !isAdmin {
				if isOwner && d.OwnerHref != "" {
					href = d.OwnerHref
				} else if d.AdminOnly {
					continue
				}
			}
			cards = append(cards, manageCard{Key: d.Key, Label: d.Label, Purpose: managePurpose[d.Key], Href: href})
		}
	}
	return cards
}

// ManageData backs manage.html.
type ManageData struct {
	Title, CurrentPage string
	Cards              []manageCard
}

// Manage serves /ui/manage, the Manage door's landing page.
func (s *Server) Manage(w http.ResponseWriter, r *http.Request) {
	s.serveManage(w, r, s.tradingNavEnabled)
}

// serveManage is Manage with the trading predicate given (a seam for tests;
// the rail uses the same predicate).
func (s *Server) serveManage(w http.ResponseWriter, r *http.Request, tradingEnabled func() bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.render(w, "manage.html", ManageData{Title: "Manage", CurrentPage: "manage-overview", Cards: s.manageCards(r, tradingEnabled)})
}
