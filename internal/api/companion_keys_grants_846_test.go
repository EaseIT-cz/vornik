package api

// GitHub #846 (design 26 §3 C7): the companion key listing carries each key's
// grants, so a managed instance's owner can see what a key may do.
//
// Found at: production (operator, 2026-11-19, demo.vornik.io: /ui/companion
// and /ui/projects/companion/keys show no key's grants).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompanionKeysList846_ShowsGrants(t *testing.T) {
	srv, _ := newCompanionServer(t)
	body := `{"projectId":"alpha","clientKind":"aws-devops-agent","sessionLabel":"ro","memoryRead":true,"skillRead":true,"delegateDisabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/companion/grant", strings.NewReader(body))
	req = req.WithContext(ContextWithProjectScope(req.Context(), "alpha"))
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var minted struct {
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &minted))
	require.NotEmpty(t, minted.Secret)

	req = httptest.NewRequest(http.MethodGet, "/api/v1/companion/keys?projectId=alpha", nil)
	req = req.WithContext(ContextWithProjectScope(req.Context(), "alpha"))
	rec = httptest.NewRecorder()
	srv.CompanionKeysList(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Keys, 1)
	k := out.Keys[0]
	for field, want := range map[string]bool{
		"memoryRead": true, "memoryWrite": false, "skillRead": true,
		"skillWrite": false, "skillAdmin": false, "delegateDisabled": true,
	} {
		got, ok := k[field].(bool)
		require.Truef(t, ok, "field %s missing from the listing: %v", field, k)
		require.Equalf(t, want, got, "field %s", field)
	}
	require.NotContains(t, rec.Body.String(), minted.Secret, "the listing never carries the secret")
}
