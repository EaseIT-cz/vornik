package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// The default rate of a caller key: a key with a workflow list and no rate
// limit of its own (#617). A caller that asks 12 questions one after another
// makes about 24 requests (a submit and a stream each).
const (
	a2aDefaultRateRPS   = 1
	a2aDefaultRateBurst = 10
)

// a2aCallerLimits bounds what one key may make an A2A agent do (#617;
// a2a-expert-federation-design.md, amendment 2026-11-06). It runs behind
// a2aScopeGuard, so the project and workflow checks have passed:
//
//   - every agent route but the public card: a key with a workflow list and
//     no rate limit of its own gets the default rate (a key's own limit is
//     applied by AuthMiddleware);
//   - POST .../tasks: 402 once the key's attributed spend reaches its
//     budget_cap_usd, and 429 once the key has created the workflow's
//     a2a.dailyQuotaPerKey tasks since 00:00 UTC. A spend or count that
//     cannot be read refuses the submit (503): the caller is external.
//
// A request without a database key row (auth off, static or admin keys)
// passes: auth off admits anyone, and the others are the operator's own.
func (s *Server) a2aCallerLimits(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/a2a/v1/agents/"), "/", 3)
		if len(parts) < 2 || (len(parts) == 3 && parts[2] == "card") {
			next(w, r)
			return
		}
		key := a2aKeyRow(r)
		if key == nil {
			next(w, r)
			return
		}
		if len(key.AllowedWorkflows) > 0 && (key.RateLimitRPS == nil || key.RateLimitBurst == nil) {
			limited := *key
			rps, burst := a2aDefaultRateRPS, a2aDefaultRateBurst
			limited.RateLimitRPS, limited.RateLimitBurst = &rps, &burst
			if enforceKeyRateLimit(w, s.apiKeyLimiter, s.rateLimitMetrics, &limited) {
				return
			}
		}
		if r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "tasks" &&
			!s.a2aSubmitAllowed(w, r, key, parts[0], parts[1]) {
			return
		}
		next(w, r)
	}
}

// a2aSubmitAllowed applies the budget and the daily quota to one submit. It
// writes the refusal and returns false when the submit must not proceed.
func (s *Server) a2aSubmitAllowed(w http.ResponseWriter, r *http.Request, key *persistence.APIKey, projectID, workflowID string) bool {
	over, spent, err := s.keySpendOverCap(r.Context(), key)
	if err != nil {
		s.logger.Warn().Err(err).Str("api_key_id", key.ID).Msg("a2a submit: budget-cap spend lookup failed; refusing")
		respondError(w, http.StatusServiceUnavailable, "BUDGET_UNAVAILABLE", "the key's spend cannot be read; try again later")
		return false
	}
	if over {
		respondError(w, http.StatusPaymentRequired, "BUDGET_EXCEEDED",
			fmt.Sprintf("key budget cap $%.4f reached (spent $%.4f)", *key.BudgetCapUSD, spent))
		return false
	}
	if s.taskRepo == nil {
		return true
	}
	quota := registry.DefaultA2ADailyQuotaPerKey
	if s.projectRegistry != nil {
		if wf := s.projectRegistry.GetWorkflow(workflowID); wf != nil {
			quota = wf.A2A.EffectiveDailyQuotaPerKey()
		}
	}
	since := utcDayStart(time.Now())
	n, err := s.taskRepo.Count(r.Context(), persistence.TaskFilter{
		ProjectID: &projectID, WorkflowID: &workflowID, CreatedByAPIKeyID: &key.ID, CreatedSince: &since,
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("api_key_id", key.ID).Msg("a2a submit: daily quota count failed; refusing")
		respondError(w, http.StatusServiceUnavailable, "QUOTA_UNAVAILABLE", "the key's daily count cannot be read; try again later")
		return false
	}
	if n >= int64(quota) {
		w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(since.Add(24*time.Hour)).Seconds())+1))
		respondError(w, http.StatusTooManyRequests, "QUOTA_EXCEEDED", "daily task quota for this workflow reached; it resets at 00:00 UTC")
		return false
	}
	return true
}

// a2aKeyRow is the request's database key row, or nil.
func a2aKeyRow(r *http.Request) *persistence.APIKey {
	id := IdentityFromContext(r.Context())
	if id == nil {
		return nil
	}
	row, _ := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey)
	return row
}

// utcDayStart is 00:00 UTC of t's UTC day: the daily quota's window start.
func utcDayStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
