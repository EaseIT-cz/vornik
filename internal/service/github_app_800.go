package service

// GitHub #800 (github-app-channel design, "Install-to-onboard"): the
// bindings an App-signed installation event writes, the one resolver that
// answers an App-backed project's App and installation, the reconciler that
// writes each bound repository's project into this node's config tree, and
// the routes the channel serves.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secretstore"
	"vornik.io/vornik/internal/templates"
)

var (
	githubAppInstallEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Name: "github_app_install_events_total",
		Help: "Repositories in App installation events, by action and outcome (GitHub #800): bound, redelivery, claimed_elsewhere, stale, capped, removed.",
	}, []string{"action", "outcome"})
	githubAppRepositories = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "vornik", Name: "github_app_repositories",
		Help: "Repositories the registered GitHub App is bound to, by state (GitHub #800): active, paused, held, removed, suspended.",
	}, []string{"state"})
	githubAppDeliveriesDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Name: "github_app_deliveries_dropped_total",
		Help: "Deliveries for a bound repository that were not routed, by why (GitHub #800).",
	}, []string{"state"})
)

// githubAppReviewTemplate is the template a bound repository's project is
// written from.
const githubAppReviewTemplate = "github-app-review"

// reconcileEvery is the reconciler's period: a node that missed an event,
// or was down for it, catches up within it.
const reconcileEvery = 5 * time.Minute

// githubAppBindings is the service side of the bindings.
type githubAppBindings struct {
	repo       persistence.GitHubAppBindingRepository
	configPath string
	configsDir string
	catalog    *templates.Catalog
	reg        *registry.Registry
	reload     func() error
	logger     zerolog.Logger
	// creds maps an App-backed project to its App, installation and
	// repository; the registry's resolver reads it.
	creds atomic.Pointer[map[string]boundCreds]
	// active is the routable rows, all every row, suspended the
	// installations' flags; refreshed with creds.
	active    atomic.Pointer[[]persistence.GitHubAppRepo]
	all       atomic.Pointer[[]persistence.GitHubAppRepo]
	suspended atomic.Pointer[map[int64]bool]
	kick      chan struct{}
}

func newGitHubAppBindings(repo persistence.GitHubAppBindingRepository, configPath, configsDir string, cat *templates.Catalog,
	reg *registry.Registry, reload func() error, logger zerolog.Logger) *githubAppBindings {
	b := &githubAppBindings{repo: repo, configPath: configPath, configsDir: configsDir, catalog: cat, reg: reg,
		reload: reload, logger: logger, kick: make(chan struct{}, 1)}
	empty := map[string]boundCreds{}
	b.creds.Store(&empty)
	return b
}

type boundCreds struct {
	registry.GitHubAppCreds
	repo string
}

// InstallationFor is the one resolver: the App-backed project's App from
// the registration, its installation from its active, unsuspended row,
// and only when the block names that row's repository.
func (b *githubAppBindings) InstallationFor(projectID, repo string) (registry.GitHubAppCreds, bool) {
	c, ok := (*b.creds.Load())[projectID]
	if !ok || c.repo != persistence.NormaliseGitHubRepo(repo) {
		return registry.GitHubAppCreds{}, false
	}
	return c.GitHubAppCreds, true
}

// refresh reloads the routable rows and the resolver's map.
func (b *githubAppBindings) refresh(ctx context.Context) error {
	regn := integrations.ReadGitHubAppRegistration(b.configPath)
	rows, err := b.repo.ListGitHubAppRepos(ctx)
	if err != nil {
		return err
	}
	susp, err := b.repo.SuspendedGitHubAppInstallations(ctx)
	if err != nil {
		return err
	}
	m := map[string]boundCreds{}
	var active []persistence.GitHubAppRepo
	counts := map[string]int{}
	for _, r := range rows {
		state := r.State
		if state == persistence.GitHubAppRepoActive && susp[r.InstallationID] {
			state = "suspended"
		}
		counts[state]++
		if regn.Registered() && state == persistence.GitHubAppRepoActive {
			active = append(active, r)
			m[r.ProjectID] = boundCreds{GitHubAppCreds: registry.GitHubAppCreds{AppID: regn.AppID, InstallationID: r.InstallationID,
				PrivateKeyPath: regn.PrivateKeyPath, APIBaseURL: regn.APIBaseURL}, repo: r.Repo}
		}
	}
	for _, st := range []string{persistence.GitHubAppRepoActive, persistence.GitHubAppRepoPaused, persistence.GitHubAppRepoHeld,
		persistence.GitHubAppRepoRemoved, "suspended"} {
		githubAppRepositories.WithLabelValues(st).Set(float64(counts[st]))
	}
	b.creds.Store(&m)
	b.active.Store(&active)
	b.all.Store(&rows)
	b.suspended.Store(&susp)
	return nil
}

var projectIDCharRE = regexp.MustCompile(`[^a-z0-9]+`)

// projectIDFor fixes a repository's project id when its row is written, so
// every node derives the same one: gh-<owner>-<name>, cut to the template's
// 32 characters; when that names a project other than this repository's
// App-backed one, six hex digits of sha256(repo) are appended.
func projectIDFor(repo string, existing func(id string) *registry.Project) string {
	r := persistence.NormaliseGitHubRepo(repo)
	id := "gh-" + strings.Trim(projectIDCharRE.ReplaceAllString(r, "-"), "-")
	if len(id) > 32 {
		id = strings.TrimRight(id[:32], "-")
	}
	if p := existing(id); p == nil || isAppBackedFor(p, r) {
		return id
	}
	sum := sha256.Sum256([]byte(r))
	if len(id) > 25 {
		id = strings.TrimRight(id[:25], "-")
	}
	return id + "-" + hex.EncodeToString(sum[:])[:6]
}

func isAppBackedFor(p *registry.Project, repo string) bool {
	return p.Forge.GitHub.UseRegistration && persistence.NormaliseGitHubRepo(p.Forge.GitHub.Repo) == repo
}

// onInstallationEvent stores a signed event (the channel's
// OnInstallationEvent), then refreshes and wakes the reconciler.
func (b *githubAppBindings) onInstallationEvent(ctx context.Context, ev github.InstallationEvent) error {
	pev := persistence.GitHubAppInstallationEvent{DeliveryID: ev.DeliveryID, InstallationID: ev.InstallationID,
		Action: ev.Action, Removed: ev.Removed}
	// An id is taken when a row of another repository, an earlier
	// repository of this event, or a project that is not this repository's
	// review project holds it (review-20261118-a33a F1: widgets-go and
	// widgets.go derive one id).
	taken := map[string]string{} // id → repo
	if rows := b.all.Load(); rows != nil {
		for _, r := range *rows {
			taken[r.ProjectID] = r.Repo
		}
	}
	for _, repo := range ev.Added {
		norm := persistence.NormaliseGitHubRepo(repo)
		id := projectIDFor(repo, func(id string) *registry.Project {
			if other, ok := taken[id]; ok && other != norm {
				return &registry.Project{ID: id} // held by another repository
			}
			return b.reg.GetProject(id)
		})
		taken[id] = norm
		pev.Added = append(pev.Added, persistence.GitHubAppRepoAdd{Repo: repo, ProjectID: id})
	}
	res, err := b.repo.ApplyInstallationEvent(ctx, pev, time.Now().UTC())
	if err != nil {
		return err
	}
	for outcome, n := range res.Outcomes {
		githubAppInstallEvents.WithLabelValues(ev.Action, outcome).Add(float64(n))
	}
	b.logger.Info().Str("action", ev.Action).Int64("installation_id", ev.InstallationID).Bool("applied", res.Applied).
		Interface("outcomes", res.Outcomes).Msg("github app: installation event stored")
	if err := b.refresh(ctx); err != nil {
		b.logger.Warn().Err(err).Msg("github app: refreshing the bindings failed; the next pass retries")
	}
	b.wake()
	return nil
}

// wake runs the reconciler now (a pass already queued is enough).
func (b *githubAppBindings) wake() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// run is the reconciler: at start, on each event, and every five minutes.
func (b *githubAppBindings) run(ctx context.Context) {
	t := time.NewTicker(reconcileEvery)
	defer t.Stop()
	for {
		b.reconcileOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.kick:
		}
	}
}

// reconcileOnce writes the project of every active row this node's
// registry lacks, then reloads once.
func (b *githubAppBindings) reconcileOnce(ctx context.Context) {
	if err := b.refresh(ctx); err != nil {
		b.logger.Warn().Err(err).Msg("github app: reading the bindings failed")
		return
	}
	if b.catalog == nil || b.configsDir == "" {
		return
	}
	m, ok := b.catalog.Get(githubAppReviewTemplate)
	if !ok {
		b.logger.Warn().Msg("github app: the github-app-review template is not installed; no project is written")
		return
	}
	wrote := false
	for _, row := range *b.active.Load() {
		if b.reg.GetProject(row.ProjectID) != nil {
			continue
		}
		if b.writeProject(m, row) {
			wrote = true
		}
	}
	if wrote && b.reload != nil {
		if err := b.reload(); err != nil {
			b.logger.Warn().Err(err).Msg("github app: reload after writing review projects failed")
		}
	}
}

// writeProject renders the row's project. An existing file is accepted
// only when it is this repository's App-backed project; otherwise nothing
// is written and the node does not route the repository.
func (b *githubAppBindings) writeProject(m templates.Manifest, row persistence.GitHubAppRepo) bool {
	rendered, err := b.catalog.MaterialiseFiles(m, map[string]string{"projectId": row.ProjectID, "displayName": row.Repo, "repo": row.Repo})
	if err != nil {
		b.logger.Warn().Err(err).Str("repo", row.Repo).Msg("github app: rendering the review project failed")
		return false
	}
	projectFile := filepath.Join(b.configsDir, "projects", row.ProjectID+".yaml")
	if raw, err := os.ReadFile(projectFile); err == nil {
		var p registry.Project
		if yaml.Unmarshal(raw, &p) != nil || !isAppBackedFor(&p, row.Repo) {
			b.logger.Warn().Str("repo", row.Repo).Str("project_id", row.ProjectID).
				Msg("github app: a project file of that id exists and is not this repository's review project; not overwritten, not routed on this node")
			return false
		}
		return false // it is ours; the next reload loads it
	}
	for target := range rendered {
		if _, err := os.Stat(filepath.Join(b.configsDir, target)); err == nil {
			delete(rendered, target) // another writer won the race; its file stands
		}
	}
	if _, err := templates.WriteRenderedFilesExclusive(b.configsDir, rendered); err != nil {
		b.logger.Warn().Err(err).Str("repo", row.Repo).Msg("github app: writing the review project failed")
		return false
	}
	b.logger.Info().Str("repo", row.Repo).Str("project_id", row.ProjectID).Msg("github app: review project written")
	return true
}

// routes is the channel's table for the active rows whose project this
// node has loaded.
func (b *githubAppBindings) routes(taskCreatorFor func(*registry.Project) github.TaskCreator) []github.RegisteredRoute {
	regn := integrations.ReadGitHubAppRegistration(b.configPath)
	if !regn.Registered() {
		return nil
	}
	raw, err := os.ReadFile(regn.PrivateKeyPath)
	if err != nil {
		b.logger.Warn().Err(err).Msg("github app: the registered App's key cannot be read; no repository is routed")
		return nil
	}
	key, err := github.LoadPrivateKeyPEM(raw)
	if err != nil {
		b.logger.Warn().Err(err).Msg("github app: the registered App's key does not parse; no repository is routed")
		return nil
	}
	var out []github.RegisteredRoute
	susp := map[int64]bool{}
	if sp := b.suspended.Load(); sp != nil {
		susp = *sp
	}
	if all := b.all.Load(); all != nil {
		for _, row := range *all {
			reason := row.State
			if row.State == persistence.GitHubAppRepoActive {
				if !susp[row.InstallationID] {
					continue // routed below, or not_provisioned
				}
				reason = "suspended"
			}
			// Bound but not routable: acked and counted, never handed to a
			// hand-made route.
			out = append(out, github.RegisteredRoute{Repo: row.Repo, DropReason: reason,
				InstallationConfig: github.InstallationConfig{InstallationID: row.InstallationID}})
		}
	}
	for _, row := range *b.active.Load() {
		p := b.reg.GetProject(row.ProjectID)
		if p == nil || !p.GitHubApp.UseRegistration {
			out = append(out, github.RegisteredRoute{Repo: row.Repo, DropReason: "not_provisioned",
				InstallationConfig: github.InstallationConfig{InstallationID: row.InstallationID}})
			continue
		}
		ic := github.InstallationConfig{ProjectID: p.ID, InstallationID: row.InstallationID, AppID: regn.AppID, PrivateKey: key,
			RepoAllowlist: []string{row.Repo}, PRReviewLabels: p.GitHubApp.PRReviewLabels, MentionHandle: p.MentionHandle()}
		if taskCreatorFor != nil {
			ic.TaskCreator = taskCreatorFor(p)
		}
		out = append(out, github.RegisteredRoute{Repo: row.Repo, InstallationConfig: ic})
	}
	return out
}

// registeredChannelConfig is the channel config a registration gives, or
// false when there is none (or its secret cannot be read).
func registeredChannelConfig(configPath string) (config.GitHubAppRegistration, string, bool) {
	regn := integrations.ReadGitHubAppRegistration(configPath)
	if !regn.Registered() {
		return regn, "", false
	}
	secret := secretstore.Value(config.GitHubAppWebhookSecretCredential)
	return regn, secret, secret != ""
}

// setupGitHubAppBindings builds the bindings and installs their resolver
// (GitHub #800). It returns the channel's registered options when an App is
// registered and its secret resolves; nil otherwise.
func (c *Container) setupGitHubAppBindings() *githubRegisteredOpts {
	if c.repos == nil || c.repos.GitHubAppBindings == nil || c.Registry == nil {
		return nil
	}
	configsDir := resolveRegistryConfigDir(c.ConfigPath)
	var cat *templates.Catalog
	if configsDir != "" {
		dir := filepath.Join(configsDir, "project-templates")
		if env := os.Getenv("VORNIK_TEMPLATES_DIR"); env != "" {
			dir = env
		}
		if loaded, err := templates.Load(dir); err == nil {
			cat = loaded
		} else {
			c.Logger.Warn().Err(err).Str("dir", dir).Msg("github app: template catalog load failed; review projects are not written")
		}
	}
	c.githubApps = newGitHubAppBindings(c.repos.GitHubAppBindings, c.ConfigPath, configsDir, cat, c.Registry,
		func() error {
			if c.ConfigReloader == nil {
				return nil
			}
			return c.ConfigReloader.Reload()
		}, c.Logger.With().Str("component", "github_app").Logger())
	if err := c.githubApps.refresh(context.Background()); err != nil {
		c.Logger.Warn().Err(err).Msg("github app: reading the bindings at boot failed")
	}
	registry.SetGitHubAppResolver(c.githubApps.InstallationFor)
	return c.githubRegisteredOpts()
}

func (c *Container) githubRegisteredOpts() *githubRegisteredOpts {
	regn, secret, ok := registeredChannelConfig(c.ConfigPath)
	if !ok || c.githubApps == nil {
		return nil
	}
	return &githubRegisteredOpts{secret: secret, apiBase: regn.APIBaseURL, onEvent: c.githubApps.onInstallationEvent,
		onDrop: func(reason string) { githubAppDeliveriesDropped.WithLabelValues(reason).Inc() }}
}

// serveGitHubWebhook is the always-mounted route: the current channel, or
// a 404 while there is none, as the unmounted route answered before.
func (c *Container) serveGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	ch := c.githubChannel.Load()
	if ch == nil {
		http.NotFound(w, r)
		return
	}
	ch.HandleWebhook(w, r)
}

// applyGitHubHotConfig runs on every registry reload: it builds the
// registered channel when an App appeared after boot with no hand-made
// channel, and swaps the bound repositories' routes.
func (c *Container) applyGitHubHotConfig(reloader *config.ConfigReloader) {
	if c.githubApps == nil {
		return
	}
	if err := c.githubApps.refresh(context.Background()); err != nil {
		c.Logger.Warn().Err(err).Msg("hot-reload: github app bindings could not be read")
		return
	}
	ch := c.githubChannel.Load()
	if ch == nil {
		opts := c.githubRegisteredOpts()
		if opts == nil {
			return
		}
		built, _, err := buildGitHubChannelRegistered(nil, c.githubTaskCreatorFor, c.Config.Integrations.AllowedHosts, c.Logger, opts)
		if err != nil || built == nil {
			msg := "github app: the registered App's channel could not be built; restart to retry"
			c.Logger.Warn().Err(err).Msg(msg)
			if reloader != nil {
				reloader.RecordReloadWarning(msg)
			}
			return
		}
		built.SetAIMarker(c.aiMarker())
		c.githubChannel.Store(built)
		c.GitHubChannel = built
		ch = built
	}
	if !ch.Registered() {
		if _, _, ok := registeredChannelConfig(c.ConfigPath); ok && reloader != nil {
			reloader.RecordReloadWarning("github app: an App was registered while hand-made github_app projects run; restart to serve it beside them")
		}
		return
	}
	ch.SetRegisteredRoutes(c.githubApps.routes(c.githubTaskCreatorFor))
}
