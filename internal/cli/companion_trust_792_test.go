package cli

// GitHub #792 (design 27 §C4): trust-github posts the trust with this
// Vornik's URL as the default audience; trusts and untrust reach the same
// project-scoped route.
//
// Found at: review (#792), observed at unit (no verb existed).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompanionTrust792_Verbs(t *testing.T) {
	var posted map[string]any
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		switch r.Method {
		case http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ktrust_1","keyId":"akey-ci","repository":"acme/app","repositoryId":4242,"jobWorkflowRef":"acme/app/.github/workflows/review.yml@refs/heads/main","events":["push"],"audience":"x"}`))
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"trusts":[]}`))
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	trustKey, trustProject, trustRepo, trustRepoID = "akey-ci", "ci-proj", "acme/app", 4242
	trustRef, trustWorkflow, trustEvents, trustAudience = "refs/heads/main", ".github/workflows/review.yml", "push", ""
	require.NoError(t, runCompanionTrustGitHub(nil, nil))
	assert.Equal(t, srv.URL, posted["audience"], "the default audience is this Vornik's URL")
	assert.Equal(t, float64(4242), posted["repositoryId"])
	assert.Equal(t, []any{"push"}, posted["events"])

	trustsProject, untrustProject = "ci-proj", "ci-proj"
	require.NoError(t, runCompanionTrusts(nil, nil))
	require.NoError(t, runCompanionUntrust(nil, []string{"ktrust_1"}))
	assert.Equal(t, []string{
		"POST /api/v1/companion/trusts",
		"GET /api/v1/companion/trusts?projectId=ci-proj",
		"DELETE /api/v1/companion/trusts?projectId=ci-proj&id=ktrust_1",
	}, seen)
}
