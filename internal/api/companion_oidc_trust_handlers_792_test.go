package api

// GitHub #792 (design 27 §C4): the trust surface creates, lists and removes
// trusts within a project the caller may reach, refuses what can never match,
// and refuses a duplicate claim set.
//
// Found at: review (#792), observed at integration (the handler over the
// SQLite repository).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

func trustServer792(t *testing.T) *Server {
	t.Helper()
	db := sqlitetest.Memory(t)
	keys := sqlite.NewAPIKeyRepository(db.DB)
	row := &persistence.APIKey{ID: "akey-ci", ProjectID: "ci-proj", Name: "ci", KeyHash: "h-ci", KeyPrefix: "sk-vornik-x", ClientKind: "github-actions"}
	require.NoError(t, keys.Create(context.Background(), row))
	plain := &persistence.APIKey{ID: "akey-plain", ProjectID: "ci-proj", Name: "plain", KeyHash: "h-plain", KeyPrefix: "sk-vornik-y"}
	require.NoError(t, keys.Create(context.Background(), plain))
	return &Server{apiKeyRepo: keys, oidcTrusts: sqlite.NewCompanionOIDCTrustRepository(db.DB)}
}

func trustReq792(method, query, body string, projects ...string) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/companion/trusts"+query, strings.NewReader(body))
	ctx := context.WithValue(r.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, projectIDKey, projects)
	return r.WithContext(ctx)
}

const goodTrust792 = `{"projectId":"ci-proj","keyId":"akey-ci","repository":"acme/app","repositoryId":4242,
	"ref":"refs/heads/main","workflow":".github/workflows/review.yml","audience":"https://vornik.example.org"}`

func TestCompanionTrusts792_CreateListDelete(t *testing.T) {
	s := trustServer792(t)
	rec := httptest.NewRecorder()
	s.CompanionTrusts(rec, trustReq792(http.MethodPost, "", goodTrust792, "ci-proj"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var got companionTrustEntry
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "acme/app/.github/workflows/review.yml@refs/heads/main", got.JobWorkflowRef)
	assert.Equal(t, []string{"push", "schedule", "workflow_dispatch"}, got.Events, "the default events")

	rec = httptest.NewRecorder()
	s.CompanionTrusts(rec, trustReq792(http.MethodPost, "", goodTrust792, "ci-proj"))
	assert.Equal(t, http.StatusConflict, rec.Code, "the same claim set twice")

	rec = httptest.NewRecorder()
	s.CompanionTrusts(rec, trustReq792(http.MethodGet, "?projectId=ci-proj", "", "ci-proj"))
	assert.Contains(t, rec.Body.String(), got.ID)

	rec = httptest.NewRecorder()
	s.CompanionTrusts(rec, trustReq792(http.MethodGet, "?projectId=ci-proj", "", "other-proj"))
	assert.Equal(t, http.StatusNotFound, rec.Code, "a caller without the project sees nothing")

	rec = httptest.NewRecorder()
	s.CompanionTrusts(rec, trustReq792(http.MethodDelete, "?projectId=ci-proj&id="+got.ID, "", "ci-proj"))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestCompanionTrusts792_Refusals(t *testing.T) {
	s := trustServer792(t)
	for name, tc := range map[string]struct {
		patch string
		code  int
	}{
		"pull_request":         {`"events":["pull_request"]`, http.StatusBadRequest},
		"unknown event":        {`"events":["issue_comment"]`, http.StatusBadRequest},
		"no repository id":     {`"repositoryId":0`, http.StatusBadRequest},
		"a non-https audience": {`"audience":"http://vornik.example.org"`, http.StatusBadRequest},
		"a ref pattern":        {`"ref":"refs/heads/*"`, http.StatusBadRequest},
		"a workflow elsewhere": {`"workflow":"ci/review.yml"`, http.StatusBadRequest},
		"a non-companion key":  {`"keyId":"akey-plain"`, http.StatusBadRequest},
		"a key elsewhere":      {`"keyId":"akey-missing"`, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.TrimSuffix(goodTrust792, "}") + "," + tc.patch + "}"
			rec := httptest.NewRecorder()
			s.CompanionTrusts(rec, trustReq792(http.MethodPost, "", body, "ci-proj"))
			assert.Equal(t, tc.code, rec.Code, rec.Body.String())
		})
	}
}

// Overlapping events with the same other claims would let row order pick the
// key: refused. A disjoint event set is a separate trust (review 3e53 F1).
func TestCompanionTrusts792_OverlappingEventsAreRefused(t *testing.T) {
	s := trustServer792(t)
	post := func(patch string) int {
		body := strings.TrimSuffix(goodTrust792, "}") + "," + patch + "}"
		rec := httptest.NewRecorder()
		s.CompanionTrusts(rec, trustReq792(http.MethodPost, "", body, "ci-proj"))
		return rec.Code
	}
	require.Equal(t, http.StatusCreated, post(`"events":["push"]`))
	assert.Equal(t, http.StatusConflict, post(`"events":["push","schedule"]`), "overlaps on push")
	assert.Equal(t, http.StatusCreated, post(`"events":["schedule"]`), "disjoint")
	assert.Equal(t, http.StatusCreated, post(`"events":["push"],"ref":"refs/heads/release"`), "another ref")
}

func TestCompanionTrusts792_ACompanionKeyCannotReachTheRoute(t *testing.T) {
	assert.False(t, isCompanionAllowedPath("/api/v1/companion/trusts"))
}
