package api

// GitHub #847 (design 26 §4): every companion key gets a default per-key
// limit, 5 per second with a burst of 50, unless its row has a limit of its
// own or api.rate_limit.companion_key.off is set. Before this change a
// companion key with no row limit (every companion key: nothing writes the
// columns for one) could call as fast as the daemon answered.
//
// Found at: review (design 26 §1 and #844's C3, 2026-11-19), observed at
// integration (the real handler admits any number of calls from one key).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/ratelimit"
)

// rateFixture is the diag fixture with a real per-key limiter and metrics.
func rateFixture(t *testing.T, cl config.CompanionKeyRateLimit) (*diagFixture, *ratelimit.Metrics) {
	t.Helper()
	f := newDiagFixture(t)
	m := ratelimit.NewMetrics(prometheus.NewRegistry())
	f.srv.apiKeyLimiter = ratelimit.NewAPIKeyLimiter()
	f.srv.rateLimitMetrics = m
	f.srv.config = &config.Config{}
	f.srv.config.API.RateLimit.CompanionKey = cl
	return f, m
}

// post sends one tools/list POST with raw as the bearer; body overrides the
// JSON-RPC body when set.
func post(f *diagFixture, raw, body string) *httptest.ResponseRecorder {
	if body == "" {
		body = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/companion", strings.NewReader(body))
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	f.srv.CompanionMCPHandler(rec, req)
	return rec
}

// admitted counts calls before the first 429, up to n.
func admitted(f *diagFixture, raw string, n int) (int, *httptest.ResponseRecorder) {
	for i := 0; i < n; i++ {
		if rec := post(f, raw, ""); rec.Code == http.StatusTooManyRequests {
			return i, rec
		}
	}
	return n, nil
}

func TestCompanionRate847_DefaultBurstThen429(t *testing.T) {
	f, m := rateFixture(t, config.CompanionKeyRateLimit{})
	n, refused := admitted(f, f.plainRaw, 60)
	assert.Equal(t, 50, n, "the default burst is 50")
	require.NotNil(t, refused, "the 51st call is refused")
	assert.NotEmpty(t, refused.Header().Get("Retry-After"))
	// The defaulted copy is never written back to the row.
	row := f.keyRepo.rows[len(f.keyRepo.rows)-1]
	assert.Nil(t, row.RateLimitRPS)
	assert.Nil(t, row.RateLimitBurst)
	// Every decision is counted: 50 admitted (allow or warn) and one block.
	allow := testutil.ToFloat64(m.DecisionsTotal.WithLabelValues(ratelimit.ScopeAPIKey, ratelimit.OutcomeAllow))
	warn := testutil.ToFloat64(m.DecisionsTotal.WithLabelValues(ratelimit.ScopeAPIKey, ratelimit.OutcomeWarn))
	block := testutil.ToFloat64(m.DecisionsTotal.WithLabelValues(ratelimit.ScopeAPIKey, ratelimit.OutcomeBlock))
	assert.Equal(t, 50.0, allow+warn)
	assert.Equal(t, 1.0, block)
}

// The check precedes the body read: a key over its limit gets 429 for a body
// that would not parse.
func TestCompanionRate847_RefusedBeforeTheBodyIsRead(t *testing.T) {
	f, _ := rateFixture(t, config.CompanionKeyRateLimit{RPS: 1, Burst: 1})
	require.Equal(t, http.StatusOK, post(f, f.plainRaw, "").Code)
	assert.Equal(t, http.StatusTooManyRequests, post(f, f.plainRaw, "not json").Code)
}

func TestCompanionRate847_ConfigReplacesTheDefaults(t *testing.T) {
	f, _ := rateFixture(t, config.CompanionKeyRateLimit{RPS: 1, Burst: 3})
	n, _ := admitted(f, f.plainRaw, 10)
	assert.Equal(t, 3, n)
}

func TestCompanionRate847_OffRemovesOnlyTheDefault(t *testing.T) {
	f, _ := rateFixture(t, config.CompanionKeyRateLimit{Off: true})
	n, _ := admitted(f, f.plainRaw, 80)
	assert.Equal(t, 80, n, "off: a key with no row limit is never refused")
}

// A row with its own limit is AuthMiddleware's to enforce
// (middleware_rate_limit_test.go); the handler adds no second, default check.
func TestCompanionRate847_ARowLimitGetsNoDefaultCheck(t *testing.T) {
	f, _ := rateFixture(t, config.CompanionKeyRateLimit{})
	row := f.keyRepo.rows[len(f.keyRepo.rows)-1]
	rps, burst := 1, 3
	row.RateLimitRPS, row.RateLimitBurst = &rps, &burst
	n, _ := admitted(f, f.plainRaw, 60)
	assert.Equal(t, 60, n)
}

func TestCompanionRate847_KeysHaveSeparateBuckets(t *testing.T) {
	f, _ := rateFixture(t, config.CompanionKeyRateLimit{})
	n, _ := admitted(f, f.plainRaw, 60)
	require.Equal(t, 50, n)
	m, _ := admitted(f, f.adminRaw, 50)
	assert.Equal(t, 50, m, "the other key is still admitted its whole burst")
}

func TestCompanionRate847_NegativeValuesAreRefusedAtLoad(t *testing.T) {
	for _, cl := range []config.CompanionKeyRateLimit{{RPS: -1}, {Burst: -1}} {
		assert.Error(t, cl.Validate(), "%+v", cl)
	}
	assert.NoError(t, config.CompanionKeyRateLimit{}.Validate())
	assert.NoError(t, config.CompanionKeyRateLimit{RPS: 2, Burst: 20}.Validate())
}
