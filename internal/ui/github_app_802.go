package ui

// GitHub #802 (github-app-channel design, "One-click App creation"): the
// console's GitHub page creates the instance's GitHub App through GitHub's
// App-manifest flow.
//
//	GET  /github           the registration, or the form
//	POST /github/manifest  a state, then an auto-submitting form to GitHub
//	GET  /github/callback  GitHub's redirect: consume the state, convert, store
//	POST /github/forget    remove the registration
//	GET  /github/installed GitHub's post-install landing (#800 builds on it)

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/persistence"
)

// manifestStateTTL is the state's life (design, Data: set here only).
const manifestStateTTL = time.Hour

// GitHubAppActions wires the page. The service fills it; nil hides the page.
type GitHubAppActions struct {
	// Origin is the resolved public origin (Config.PublicOrigin:
	// server.public_base_url, else auth.external_base_url).
	Origin func() string
	// Registration reads the registration as it is on disk now.
	Registration func() config.GitHubAppRegistration
	States       persistence.GitHubAppManifestStateRepository
	// Convert calls GitHub's conversion through the dial-guarded client.
	Convert func(ctx context.Context, apiBase, code string) (*github.ManifestApp, error)
	// StoreReady reports whether the credential store can take the secret.
	StoreReady func() bool
	Register   func(ctx context.Context, app *github.ManifestApp, apiBase string, who persistence.AdminAuditEntry) error
	Forget     func(ctx context.Context, who persistence.AdminAuditEntry) error
	// Count records vornik_github_app_manifest_total{outcome}; nil is a no-op.
	Count func(outcome string)
	// Repos lists the bound repositories, SetRepoState moves one between
	// states, and Changed refreshes the routes and wakes the reconciler
	// after a move (GitHub #800). Nil Repos hides the table.
	Repos        func(ctx context.Context) ([]persistence.GitHubAppRepo, error)
	SetRepoState func(ctx context.Context, repo string, from []string, to string) (bool, error)
	Changed      func()
	Now          func() time.Time
}

// WithGitHubApp wires the GitHub page (GitHub #802).
func WithGitHubApp(a *GitHubAppActions) ServerOption {
	return func(s *Server) { s.githubApp = a }
}

// GitHubAppPageData backs github_app.html.
type GitHubAppPageData struct {
	adminCommonData
	Registration config.GitHubAppRegistration
	InstallURL   string
	// Problem, when set, is why the form is not offered.
	Problem string
	// Repos are the bound repositories (GitHub #800).
	Repos []persistence.GitHubAppRepo
	Flash string
	Error string
}

// GitHubAppRedirectData backs github_app_redirect.html. Title is read by
// pageHead.
type GitHubAppRedirectData struct {
	Title    string
	Action   string
	Manifest string
}

func (a *GitHubAppActions) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *GitHubAppActions) count(outcome string) {
	if a.Count != nil {
		a.Count(outcome)
	}
}

func hash802(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// GitHubAppRouter serves the five routes.
func (s *Server) GitHubAppRouter(w http.ResponseWriter, r *http.Request) {
	a := s.githubApp
	if a == nil {
		http.NotFound(w, r)
		return
	}
	switch api.OperatorSurfaceAccessFor(r, s.isAdminKeyForSurface, api.SurfaceGitHubApp) {
	case api.OperatorSurfaceAllowed:
	case api.OperatorSurfaceRefused:
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/github":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		flash := ""
		switch {
		case r.URL.Query().Get("created") != "":
			flash = "The GitHub App is created. Install it on the repositories Vornik should review."
		case r.URL.Query().Get("forgotten") != "":
			flash = "Vornik no longer uses that App. Delete it on GitHub as well, from the App's settings."
		}
		s.renderGitHubApp(w, r, http.StatusOK, flash, "")
	case "/github/installed":
		s.renderGitHubApp(w, r, http.StatusOK, "The App is installed. GitHub will now send Vornik the repositories' events.", "")
	case "/github/manifest":
		s.githubAppManifest(w, r, a)
	default:
		if strings.HasPrefix(r.URL.Path, "/github/repos/") {
			s.githubAppRepoAction(w, r, a)
			return
		}
		s.githubAppRouterRest(w, r, a)
	}
}

func (s *Server) githubAppRouterRest(w http.ResponseWriter, r *http.Request, a *GitHubAppActions) {
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/github/callback":
		s.githubAppCallback(w, r, a)
	case "/github/forget":
		if !postSameOrigin802(w, r) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		if r.ParseForm() != nil || r.PostFormValue("confirm") != "yes" {
			s.renderGitHubApp(w, r, http.StatusBadRequest, "", "Tick the box to confirm.")
			return
		}
		if err := a.Forget(r.Context(), credentialsWho(r, credentialsActor(r))); err != nil {
			s.logger.Warn().Err(err).Msg("github app: forget failed")
			s.renderGitHubApp(w, r, http.StatusInternalServerError, "", "The App could not be forgotten; try again.")
			return
		}
		http.Redirect(w, r, "/ui/github?forgotten=1", http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

func postSameOrigin802(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if err := approval.CheckRequest(r); err != nil {
		approval.WriteError(w, r, err)
		return false
	}
	return true
}

// formProblem says why the form cannot be offered, or "".
func (a *GitHubAppActions) formProblem() string {
	if _, err := github.BuildManifest(a.Origin()); err != nil {
		return "Set server.public_base_url (or auth.external_base_url) to this instance's public https address first: GitHub sends the App's events there."
	}
	if a.StoreReady != nil && !a.StoreReady() {
		return "The credential store is not available, so the App's webhook secret could not be kept. Run the doctor (secret_store_key)."
	}
	return ""
}

func (s *Server) renderGitHubApp(w http.ResponseWriter, r *http.Request, status int, flash, errText string) {
	a := s.githubApp
	data := GitHubAppPageData{
		adminCommonData: adminCommonData{Title: "GitHub", CurrentPage: "github",
			IsAdmin: api.IsAdminClassRequest(r, s.operatorCapability.IsAdminKey)},
		Registration: a.Registration(),
		Flash:        flash,
		Error:        errText,
	}
	if data.Registration.Registered() {
		if u := strings.TrimRight(data.Registration.HTMLURL, "/"); strings.HasPrefix(u, "https://") {
			data.InstallURL = u + "/installations/new"
		}
		if a.Repos != nil {
			repos, err := a.Repos(r.Context())
			if err != nil {
				s.logger.Warn().Err(err).Msg("github app: listing the bound repositories failed")
			}
			data.Repos = repos
		}
	} else {
		data.Problem = a.formProblem()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.templates.ExecuteTemplate(w, "github_app.html", data); err != nil {
		s.logger.Error().Err(err).Msg("github app: render failed")
	}
}

func (s *Server) githubAppManifest(w http.ResponseWriter, r *http.Request, a *GitHubAppActions) {
	if !postSameOrigin802(w, r) {
		return
	}
	// The flow ends in a browser redirect from GitHub, which carries a
	// session cookie and nothing else.
	session := api.SessionIDFromContext(r.Context())
	if session == "" {
		http.Error(w, "sign in to the console to create a GitHub App", http.StatusForbidden)
		return
	}
	if a.Registration().Registered() {
		a.count("exists")
		s.renderGitHubApp(w, r, http.StatusConflict, "", "A GitHub App is already registered. Forget it first to create another.")
		return
	}
	if p := a.formProblem(); p != "" {
		s.renderGitHubApp(w, r, http.StatusConflict, "", p)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	org := ""
	if r.PostFormValue("owner") == "organization" {
		org = strings.TrimSpace(r.PostFormValue("org"))
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "could not start", http.StatusInternalServerError)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	action, apiBase, err := github.ManifestEndpoints(r.PostFormValue("enterprise_url"), org, state)
	if err != nil {
		s.renderGitHubApp(w, r, http.StatusBadRequest, "", "Check the organisation name or the GitHub Enterprise address: "+err.Error())
		return
	}
	manifest, err := github.BuildManifest(a.Origin())
	if err != nil {
		s.renderGitHubApp(w, r, http.StatusConflict, "", "The public address is not usable: "+err.Error())
		return
	}
	now := a.now()
	if err := a.States.PutManifestState(r.Context(), hash802(state), hash802(session), apiBase, now, now.Add(manifestStateTTL)); err != nil {
		s.logger.Warn().Err(err).Msg("github app: storing the state failed")
		http.Error(w, "could not start; try again", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "github_app_redirect.html", GitHubAppRedirectData{Title: "Creating the GitHub App", Action: action, Manifest: string(manifest)}); err != nil {
		s.logger.Error().Err(err).Msg("github app: render failed")
	}
}

func (s *Server) githubAppCallback(w http.ResponseWriter, r *http.Request, a *GitHubAppActions) {
	// A cross-site GET from github.com: no same-origin check. The state,
	// bound to this session and consumed once, is the CSRF defence.
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	refused := func(outcome string) {
		a.count(outcome)
		s.renderGitHubApp(w, r, http.StatusBadRequest, "",
			"This link has expired, was already used, or was started in another browser session. Start again.")
	}
	session := api.SessionIDFromContext(r.Context())
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if session == "" || state == "" || code == "" {
		refused(string(persistence.ManifestStateMissing))
		return
	}
	sh, ssh, now := hash802(state), hash802(session), a.now()
	class, err := a.States.ClassifyManifestState(r.Context(), sh, ssh, now)
	if err != nil {
		// A store fault, not a bad link: its own outcome. The consume below
		// still decides.
		s.logger.Warn().Err(err).Msg("github app: reading the state failed")
		class = "state_error"
	}
	apiBase, ok, err := a.States.ConsumeManifestState(r.Context(), sh, ssh, now)
	if err != nil || !ok {
		if class == persistence.ManifestStateLive {
			class = persistence.ManifestStateUsed // lost a race to a concurrent consume
		}
		refused(string(class))
		return
	}
	if a.Registration().Registered() {
		a.count("exists")
		s.renderGitHubApp(w, r, http.StatusConflict, "", "A GitHub App is already registered. Forget it first to create another.")
		return
	}
	app, err := a.Convert(r.Context(), apiBase, code)
	if err != nil {
		a.count("conversion_failed")
		s.logger.Warn().Err(err).Msg("github app: conversion failed")
		s.renderGitHubApp(w, r, http.StatusBadGateway, "", "GitHub did not hand over the new App: "+err.Error())
		return
	}
	if err := a.Register(r.Context(), app, apiBase, credentialsWho(r, credentialsActor(r))); err != nil {
		if errors.Is(err, integrations.ErrGitHubAppExists) {
			a.count("exists")
			s.renderGitHubApp(w, r, http.StatusConflict, "",
				"A GitHub App was registered meanwhile, so this new one is not used. Delete it on GitHub ("+app.HTMLURL+").")
			return
		}
		a.count("store_failed")
		s.logger.Warn().Err(err).Msg("github app: storing the App failed")
		s.renderGitHubApp(w, r, http.StatusInternalServerError, "",
			"GitHub created the App, but Vornik could not keep its credentials. Delete the App on GitHub ("+app.HTMLURL+") and start again.")
		return
	}
	a.count("created")
	s.logger.Info().Int64("app_id", app.ID).Str("slug", app.Slug).Msg("github app: registered")
	http.Redirect(w, r, "/ui/github?created=1", http.StatusSeeOther)
}

// githubAppRepoAction serves POST /github/repos/<owner>/<name>/{pause,
// resume,provision} (GitHub #800). Resume of a removed row is refused:
// GitHub decides that state.
func (s *Server) githubAppRepoAction(w http.ResponseWriter, r *http.Request, a *GitHubAppActions) {
	if a.SetRepoState == nil {
		http.NotFound(w, r)
		return
	}
	if !postSameOrigin802(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/github/repos/")
	i := strings.LastIndex(rest, "/")
	if i <= 0 {
		http.NotFound(w, r)
		return
	}
	repo, action := rest[:i], rest[i+1:]
	if !github.ValidRepoName(repo) {
		http.NotFound(w, r)
		return
	}
	moves := map[string]struct {
		from []string
		to   string
	}{
		"pause":     {[]string{persistence.GitHubAppRepoActive}, persistence.GitHubAppRepoPaused},
		"resume":    {[]string{persistence.GitHubAppRepoPaused}, persistence.GitHubAppRepoActive},
		"provision": {[]string{persistence.GitHubAppRepoHeld}, persistence.GitHubAppRepoActive},
	}
	m, ok := moves[action]
	if !ok {
		http.NotFound(w, r)
		return
	}
	done, err := a.SetRepoState(r.Context(), repo, m.from, m.to)
	switch {
	case err != nil:
		s.logger.Warn().Err(err).Str("repo", repo).Msg("github app: changing a repository's state failed")
		s.renderGitHubApp(w, r, http.StatusInternalServerError, "", "That could not be saved; try again.")
		return
	case !done:
		s.renderGitHubApp(w, r, http.StatusConflict, "", "That repository is not in a state this button changes.")
		return
	}
	if a.Changed != nil {
		a.Changed()
	}
	http.Redirect(w, r, "/ui/github", http.StatusSeeOther)
}
