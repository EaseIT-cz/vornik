package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Grant-time rules of the broker design §8 and §9. Refusing at grant, not at
// first use, is the same principle the handler already applies to unknown
// workflow IDs: a key that cannot do what it was minted for is a dead key the
// operator finds out about mid-conversation.

func brokerGrant(t *testing.T, body string) (*httptest.ResponseRecorder, *memAPIKeyRepo) {
	t.Helper()
	repo := &memAPIKeyRepo{}
	srv := &Server{logger: zerolog.Nop(), apiKeyRepo: repo, projectRegistry: seedBrokerRegistry(t)}
	req := withAuthDisabled(httptest.NewRequest(http.MethodPost, "/api/v1/companion/grant", strings.NewReader(body)))
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, req)
	return rec, repo
}

func TestCompanionGrant_AcceptsFrontAgentClientKinds(t *testing.T) {
	for _, kind := range []string{"hermes", "openclaw"} {
		rec, _ := brokerGrant(t, `{"projectId":"memory-acme","clientKind":"`+kind+`","memoryRead":true}`)
		require.Equal(t, http.StatusCreated, rec.Code, "%s: %s", kind, rec.Body.String())
	}
}

func TestCompanionGrant_NoDelegatePersistsAndEchoes(t *testing.T) {
	rec, repo := brokerGrant(t, `{"projectId":"memory-acme","clientKind":"hermes","memoryRead":true,"memoryWrite":true,"delegateDisabled":true}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Len(t, repo.rows, 1)
	require.True(t, repo.rows[0].DelegateDisabled)
	require.Contains(t, rec.Body.String(), `"delegateDisabled":true`)
}

func TestCompanionGrant_BrokerProjectRules(t *testing.T) {
	cases := []struct {
		name, body, code string
	}{
		{"memory on broker project", `{"projectId":"broker-acme","clientKind":"hermes","memoryRead":true}`, "BROKER_PROJECT"},
		{"skills on broker project", `{"projectId":"broker-acme","clientKind":"hermes","skillRead":true}`, "BROKER_PROJECT"},
		{"no-delegate on broker project", `{"projectId":"broker-acme","clientKind":"hermes","delegateDisabled":true}`, "BROKER_PROJECT"},
		{"non-broker workflow on broker project", `{"projectId":"broker-acme","clientKind":"hermes","allowedWorkflows":["wf-plain"]}`, "BROKER_PROJECT"},
		{"broker workflow on ordinary project", `{"projectId":"memory-acme","clientKind":"hermes","allowedWorkflows":["mail-digest"]}`, "BROKER_WORKFLOW"},
		{"no-delegate with a workflow list", `{"projectId":"memory-acme","clientKind":"hermes","delegateDisabled":true,"allowedWorkflows":["wf-plain"]}`, "VALIDATION_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, repo := brokerGrant(t, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), tc.code)
			require.Empty(t, repo.rows, "a refused grant must not mint")
		})
	}
}

func TestCompanionGrant_BrokerProjectMintsDelegateKey(t *testing.T) {
	rec, repo := brokerGrant(t, `{"projectId":"broker-acme","clientKind":"hermes","allowedWorkflows":["mail-digest"],"budgetCapUsd":5}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Len(t, repo.rows, 1)
	require.False(t, repo.rows[0].MemoryRead)
}
