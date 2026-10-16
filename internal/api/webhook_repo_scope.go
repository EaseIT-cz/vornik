package api

import (
	"context"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Outcomes of the webhook repository-scope gate. Every delivery that reaches
// the gate is counted under exactly one, so the four sum to the deliveries
// examined (GitHub #407).
const (
	webhookRepoScopeAllowed      = "allowed"
	webhookRepoScopeDropped      = "dropped"
	webhookRepoScopeNoRepository = "no_repository"
	webhookRepoScopeUnrestricted = "unrestricted"
)

// webhookRepoScopeOutcome classifies a delivery's repository against a
// source's effective scope. repo is the delivery's repository.full_name ("" when
// the event carries none).
func webhookRepoScopeOutcome(scope registry.WebhookRepoScope, repo string) string {
	switch {
	case scope.Unrestricted():
		return webhookRepoScopeUnrestricted
	case repo == "":
		return webhookRepoScopeNoRepository
	case scope.Allows(repo):
		return webhookRepoScopeAllowed
	default:
		return webhookRepoScopeDropped
	}
}

// dropOutOfScopeRepo applies the source's repository scope and reports whether
// it answered the request. An event for a repository outside the scope is
// acknowledged 200 "filtered" (the provider must not retry a correct
// decision), recorded as a filtered webhook_events row naming the repository,
// and logged at warn: the installation or webhook reaches further than the
// project. An event with no repository passes to the remaining gates.
// see LLD § https://docs.vornik.io "Generic webhook sources: repository scope (2026-10-16, GitHub #407)"
func (s *Server) dropOutOfScopeRepo(ctx context.Context, w http.ResponseWriter, project *registry.Project, source registry.ProjectWebhookSource, event map[string]any, body []byte, deliveryID string) bool {
	// valueAtPath renders a non-string full_name too, so a malformed value
	// cannot match an owner/repo entry and a scoped source drops it (fails
	// closed); only an absent or empty one reads as "no repository".
	repo := strings.TrimSpace(valueAtPath(event, "repository.full_name"))
	outcome := webhookRepoScopeOutcome(source.RepoScope(project), repo)
	s.apiMetrics.RecordWebhookRepoScope(project.ID, source.Name, outcome)
	if outcome != webhookRepoScopeDropped {
		return false
	}
	eventID := webhookEventID(event, source.EventIDPath, deliveryID, body)
	s.logger.Warn().
		Str("project_id", project.ID).
		Str("source", source.Name).
		Str("repo", repo).
		Str("event_id", eventID).
		Msg("webhook delivery dropped: repository is outside the source's repository scope (narrow the App installation or set webhooks.sources[].repos)")
	s.recordWebhookEvent(ctx, project.ID, source.Name, eventID, body, persistence.WebhookEventStatusFiltered, nil,
		"repo_not_in_scope", "repository "+repo+" is not in this source's repository scope")
	respondJSON(w, http.StatusOK, map[string]string{"status": "filtered"})
	return true
}
