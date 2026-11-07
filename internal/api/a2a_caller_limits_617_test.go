package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// #617 (federation design, amendment 2026-11-06): an A2A caller key was
// bounded by nothing on the A2A path. Its budget cap was never read there,
// a key with a workflow list had no rate limit unless one was set by hand
// (none of the 3 such keys measured had one), and there was no daily quota.

const a2aSubmitPath = "/a2a/v1/agents/expert/qa/tasks"

type spendStub struct {
	persistence.TaskLLMUsageRepository
	spend float64
	err   error
}

func (s spendStub) SumCostByAPIKey(context.Context, string, time.Time, time.Time) (float64, error) {
	return s.spend, s.err
}

type limitsFixture struct {
	srv    *Server
	counts []persistence.TaskFilter
	count  int64
	cntErr error
}

func newLimitsFixture(t *testing.T, quota int) *limitsFixture {
	t.Helper()
	f := &limitsFixture{}
	reg := registry.New()
	require.NoError(t, reg.RegisterTransient("qa", &registry.Workflow{
		ID: "qa", A2A: registry.WorkflowA2A{Publish: true, DailyQuotaPerKey: quota},
	}))
	repo := &mocks.MockTaskRepository{
		CountFunc: func(_ context.Context, fl persistence.TaskFilter) (int64, error) {
			f.counts = append(f.counts, fl)
			return f.count, f.cntErr
		},
	}
	f.srv = NewServer(
		WithTaskRepository(repo),
		WithProjectRegistry(reg),
		WithAPIKeyLimiter(ratelimit.NewAPIKeyLimiter()),
		WithLLMUsageRepository(spendStub{}),
	)
	return f
}

func callerKey(id string) *persistence.APIKey {
	return &persistence.APIKey{ID: id, ProjectID: "expert", AllowedWorkflows: []string{"qa"}}
}

func (f *limitsFixture) do(t *testing.T, method, path string, key *persistence.APIKey) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if key != nil {
		ctx := context.WithValue(req.Context(), identityKey, &auth.Identity{
			BoundProjectID: key.ProjectID,
			Extra:          map[string]any{auth.ExtraDBKeyRow: key},
		})
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	f.srv.a2aCallerLimits(passThroughOK)(rec, req)
	return rec
}

func TestA2ACallerLimits_BudgetCapRefusesSubmit_617(t *testing.T) {
	f := newLimitsFixture(t, 0)
	capUSD := 1.0
	key := callerKey("k-budget")
	key.BudgetCapUSD = &capUSD

	f.srv.llmUsageRepo = spendStub{spend: 0.4}
	assert.Equal(t, http.StatusOK, f.do(t, http.MethodPost, a2aSubmitPath, key).Code, "under the cap")

	f.srv.llmUsageRepo = spendStub{spend: 1.0}
	rec := f.do(t, http.MethodPost, a2aSubmitPath, key)
	assert.Equal(t, http.StatusPaymentRequired, rec.Code)
	assert.Contains(t, rec.Body.String(), "BUDGET_EXCEEDED")

	f.srv.llmUsageRepo = spendStub{err: errors.New("db down")}
	assert.Equal(t, http.StatusServiceUnavailable, f.do(t, http.MethodPost, a2aSubmitPath, key).Code,
		"an unreadable spend refuses an external submit")
}

func TestA2ACallerLimits_DailyQuota_617(t *testing.T) {
	f := newLimitsFixture(t, 3)
	key := callerKey("k-quota")

	f.count = 2
	require.Equal(t, http.StatusOK, f.do(t, http.MethodPost, a2aSubmitPath, key).Code, "the third task of the day passes")
	require.Len(t, f.counts, 1)
	got := f.counts[0]
	require.NotNil(t, got.CreatedByAPIKeyID)
	require.NotNil(t, got.WorkflowID)
	require.NotNil(t, got.ProjectID)
	require.NotNil(t, got.CreatedSince)
	assert.Equal(t, "k-quota", *got.CreatedByAPIKeyID)
	assert.Equal(t, "qa", *got.WorkflowID)
	assert.Equal(t, "expert", *got.ProjectID)
	assert.Equal(t, utcDayStart(time.Now()), *got.CreatedSince, "the window starts at 00:00 UTC")

	f.count = 3
	rec := f.do(t, http.MethodPost, a2aSubmitPath, key)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Contains(t, rec.Body.String(), "QUOTA_EXCEEDED")

	f.cntErr = errors.New("db down")
	assert.Equal(t, http.StatusServiceUnavailable, f.do(t, http.MethodPost, a2aSubmitPath, key).Code)
}

func TestA2ACallerLimits_DefaultQuotaIs50_617(t *testing.T) {
	f := newLimitsFixture(t, 0)
	f.count = 49
	assert.Equal(t, http.StatusOK, f.do(t, http.MethodPost, a2aSubmitPath, callerKey("k-def")).Code)
	f.count = 50
	assert.Equal(t, http.StatusTooManyRequests, f.do(t, http.MethodPost, a2aSubmitPath, callerKey("k-def")).Code)
}

// The quota resets at 00:00 UTC: the window of a request one second after
// midnight starts at that midnight, so the previous day's tasks are outside.
func TestUTCDayStart_617(t *testing.T) {
	prague := time.FixedZone("CET", 3600)
	before := time.Date(2026, 11, 6, 23, 59, 59, 0, time.UTC)
	after := before.Add(2 * time.Second)
	assert.Equal(t, time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC), utcDayStart(before))
	assert.Equal(t, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), utcDayStart(after))
	assert.Equal(t, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), utcDayStart(after.In(prague)),
		"the day is UTC's, not the server's zone")
}

func TestA2ACallerLimits_DefaultRateForAListedKey_617(t *testing.T) {
	f := newLimitsFixture(t, 0)
	key := callerKey("k-rate")
	path := "/a2a/v1/agents/expert/qa/tasks/t1" // a stream: rate only
	for i := 0; i < 10; i++ {
		require.Equal(t, http.StatusOK, f.do(t, http.MethodGet, path, key).Code, "request %d is within the burst of 10", i+1)
	}
	rec := f.do(t, http.MethodGet, path, key)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "the 11th request in a second is over the default")
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	assert.Empty(t, f.counts, "a stream request never counts the quota")
}

func TestA2ACallerLimits_OwnRateReplacesTheDefault_617(t *testing.T) {
	f := newLimitsFixture(t, 0)
	key := callerKey("k-own")
	rps, burst := 100, 100
	key.RateLimitRPS, key.RateLimitBurst = &rps, &burst
	for i := 0; i < 20; i++ {
		require.Equal(t, http.StatusOK, f.do(t, http.MethodGet, "/a2a/v1/agents/expert/qa/tasks/t1", key).Code,
			"a key with its own limit is limited by the middleware, not the default (request %d)", i+1)
	}
}

func TestA2ACallerLimits_CardAndUnkeyedRequestsPass_617(t *testing.T) {
	f := newLimitsFixture(t, 0)
	capUSD := 0.01
	key := callerKey("k-card")
	key.BudgetCapUSD = &capUSD
	f.srv.llmUsageRepo = spendStub{spend: 99}
	f.count = 999
	for i := 0; i < 20; i++ {
		require.Equal(t, http.StatusOK, f.do(t, http.MethodGet, "/a2a/v1/agents/expert/qa/card", key).Code, "the card is public")
	}
	assert.Equal(t, http.StatusOK, f.do(t, http.MethodPost, a2aSubmitPath, nil).Code, "auth off: no key row, no key limits")
}

// A key without a workflow list is a full project key: no default rate,
// but its budget and the quota still bound what it submits over A2A.
func TestA2ACallerLimits_UnlistedKeyGetsBudgetAndQuotaOnly_617(t *testing.T) {
	f := newLimitsFixture(t, 0)
	key := &persistence.APIKey{ID: "k-full", ProjectID: "expert"}
	for i := 0; i < 20; i++ {
		require.Equal(t, http.StatusOK, f.do(t, http.MethodGet, "/a2a/v1/agents/expert/qa/tasks/t1", key).Code)
	}
	f.count = 50
	assert.Equal(t, http.StatusTooManyRequests, f.do(t, http.MethodPost, a2aSubmitPath, key).Code)
}
