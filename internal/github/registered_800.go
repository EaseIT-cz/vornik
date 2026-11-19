package github

// GitHub #800 (github-app-channel design, "Install-to-onboard"): the
// channel's registered mode. Installation events are handed to the
// service, which writes the bindings; bound repositories route through a
// table the service swaps on every reload.

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
)

// InstallationEvent is a signed installation or installation_repositories
// delivery, reduced to what the bindings need.
type InstallationEvent struct {
	DeliveryID     string
	InstallationID int64
	Action         string
	Added          []string // owner/name as GitHub sent it
	Removed        []string
}

// RegisteredRoute is one bound repository: its route is a full
// InstallationConfig (the registration's App, the binding's installation).
type RegisteredRoute struct {
	Repo string // owner/name; matched case-insensitively
	// DropReason, when set, marks a bound repository that is not routed
	// (paused, held, removed, suspended, not_provisioned): its deliveries
	// are acked and counted, never handed to another route.
	DropReason string
	InstallationConfig
}

type registeredRoutes struct {
	byRepo atomic.Pointer[map[string]*installation]
}

// repoNameRE is GitHub's owner/name rule.
var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// ValidRepoName reports whether repo is a GitHub owner/name.
func ValidRepoName(repo string) bool { return repoNameRE.MatchString(repo) }

// SetRegisteredRoutes swaps the bound repositories' table whole, so a
// delivery never sees a torn one. Called on every registry reload.
func (c *Channel) SetRegisteredRoutes(routes []RegisteredRoute) {
	if c == nil {
		return
	}
	m := make(map[string]*installation, len(routes))
	for _, rt := range routes {
		inst := buildInstallation(rt.InstallationConfig)
		inst.registered, inst.dropReason = true, rt.DropReason
		m[strings.ToLower(strings.TrimSpace(rt.Repo))] = inst
	}
	c.registered.byRepo.Store(&m)
}

// routeRegistered answers whether the delivery's repository is bound
// (decided) and, if so, its installation; nil means bound but dropped.
func (c *Channel) routeRegistered(p eventPayload) (*installation, bool) {
	if !c.cfg.Registered {
		return nil, false
	}
	mp := c.registered.byRepo.Load()
	if mp == nil {
		return nil, false
	}
	inst, ok := (*mp)[strings.ToLower(p.Repository.FullName)]
	if !ok {
		return nil, false
	}
	if inst.installationID != p.Installation.ID {
		c.drop("other_installation", p)
		return nil, true
	}
	if inst.dropReason != "" {
		c.drop(inst.dropReason, p)
		return nil, true
	}
	return inst, true
}

func (c *Channel) drop(reason string, p eventPayload) {
	c.logger.Info().Str("reason", reason).Str("repo", p.Repository.FullName).Int64("installation_id", p.Installation.ID).
		Msg("github-app: delivery for a bound repository not routed")
	if c.cfg.OnDrop != nil {
		c.cfg.OnDrop(reason)
	}
}

// dispatchRegistered runs the review and CI paths for a bound repository.
// Mention replies and label-triggered tasks stay off for App-backed
// projects: a public repository's every commenter could start one.
func (c *Channel) dispatchRegistered(ctx context.Context, event, delivery string, p eventPayload, inst *installation) {
	switch event {
	case "pull_request":
		if kind, ok := reviewKindForAction(p.Action); ok {
			c.handlePullRequestReview(ctx, event, delivery, p, inst, kind)
		}
	case "workflow_run":
		if p.Action == "completed" {
			c.handleWorkflowRunCompleted(ctx, event, delivery, p, inst)
		}
	default:
		c.logger.Debug().Str("event", event).Str("action", p.Action).Msg("github-app: event not handled for an App-backed project, acking")
	}
}

type installationPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repositories []struct {
		FullName string `json:"full_name"`
	} `json:"repositories"`
	RepositoriesAdded []struct {
		FullName string `json:"full_name"`
	} `json:"repositories_added"`
	RepositoriesRemoved []struct {
		FullName string `json:"full_name"`
	} `json:"repositories_removed"`
}

// handleInstallationEvent parses the event and hands it to the service. A
// name that is not GitHub's owner/name rule is dropped from the event.
func (c *Channel) handleInstallationEvent(w http.ResponseWriter, r *http.Request, event, delivery string, body []byte) {
	var p installationPayload
	if err := json.Unmarshal(body, &p); err != nil || p.Installation.ID == 0 || delivery == "" {
		http.Error(w, "invalid installation event", http.StatusBadRequest)
		return
	}
	names := func(list []struct {
		FullName string `json:"full_name"`
	}) []string {
		var out []string
		for _, x := range list {
			if ValidRepoName(x.FullName) {
				out = append(out, x.FullName)
			} else {
				c.logger.Warn().Str("repo", x.FullName).Msg("github-app: installation event names a repository outside GitHub's rule; skipped")
			}
		}
		return out
	}
	ev := InstallationEvent{DeliveryID: delivery, InstallationID: p.Installation.ID, Action: p.Action}
	switch {
	case event == "installation" && p.Action == "created":
		ev.Added = names(p.Repositories)
	case event == "installation_repositories" && p.Action == "added":
		ev.Added = names(p.RepositoriesAdded)
	case event == "installation_repositories" && p.Action == "removed":
		ev.Removed = names(p.RepositoriesRemoved)
	}
	if c.cfg.OnInstallationEvent == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := c.cfg.OnInstallationEvent(r.Context(), ev); err != nil {
		c.logger.Error().Err(err).Str("event", event).Str("action", p.Action).Str("delivery", delivery).
			Msg("github-app: storing the installation event failed; redeliver it from the App's settings")
		http.Error(w, "could not store the installation event", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Registered reports whether the channel serves the registered App.
func (c *Channel) Registered() bool { return c != nil && c.cfg.Registered }
