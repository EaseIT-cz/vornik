package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #86 / LLD 21 §D2 "The generic harness contract" (2026-10-08, D6):
// a new harness needs no daemon change. The grant handler refused every
// clientKind outside a closed list, so a harness nobody had written an
// adapter for could not be granted a key without a daemon release. Any kind
// matching ^[a-z][a-z0-9-]{1,31}$ is now accepted; the unlisted ones are
// classed shell_capable (agentadmin.HarnessClassOf), the conservative claims.

func grantWithKind(t *testing.T, kind string) *httptest.ResponseRecorder {
	t.Helper()
	srv, _ := newCompanionServer(t)
	body, _ := json.Marshal(map[string]any{"projectId": "alpha", "clientKind": kind})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/companion/grant", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, withAuthDisabled(req))
	return rec
}

func TestCompanionGrant_UnknownPatternValidKindAccepted(t *testing.T) {
	for _, kind := range []string{"goose", "claude-cli", "gemini-cli", "a1", "x-" + strings.Repeat("y", 30)} {
		rec := grantWithKind(t, kind)
		require.Equal(t, http.StatusCreated, rec.Code, "kind %q: %s", kind, rec.Body.String())
		var resp companionGrantResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, kind, resp.ClientKind)
	}
}

func TestCompanionGrant_MalformedKindRefused(t *testing.T) {
	for _, kind := range []string{"Goose!", "Goose", "1goose", "g", "goose_cli", "goose cli", "x" + strings.Repeat("y", 32), "-goose"} {
		rec := grantWithKind(t, kind)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "kind %q: %s", kind, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "^[a-z][a-z0-9-]{1,31}$", "kind %q: the refusal names the pattern", kind)
	}
	// Empty keeps its own message.
	rec := grantWithKind(t, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "clientKind is required")
}
