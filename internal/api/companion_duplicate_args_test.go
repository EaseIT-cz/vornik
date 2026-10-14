package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// Duplicate argument keys (security review of 6035e952d, 2026-10-14).
//
// Found at: review. A user (or an audit reader) observes it in the companion
// tool audit: the handler decodes arguments into a Go map or struct, where a
// repeated key keeps its LAST value, while the audit row stores the arguments
// as sent. {"project":"beta","project":"alpha"} was checked and admitted as
// "alpha" (the key's own project) while the audit showed "beta" first — a
// parser differential between what was decided and what was recorded, and
// the same for every declared argument. A repeated key is never legitimate in
// a tool call, so it is refused before any other check.
// callToolJSON sends delegate with its arguments as raw JSON text, so a
// repeated key reaches the handler unchanged (a Go map cannot hold one).
func callToolJSON(t *testing.T, srv *Server, raw, args string) (string, bool) {
	t.Helper()
	req := withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{"name": "delegate", "arguments": json.RawMessage(args)}), raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	return decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
}

func TestCompanionArgs_DuplicateKeyRefusedOnLegacyName(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	text, isErr := callToolJSON(t, srv, raw,
		`{"workflow":"wf-alpha","prompt":"p","project":"beta","project":"alpha"}`)
	require.True(t, isErr, "a repeated project key was admitted: %s", text)
	assert.Contains(t, text, `"project"`)
	assert.Contains(t, text, "more than once")
	assert.Equal(t, 0, taskRepo.CallCount.Create, "no task may be created")
}

func TestCompanionArgs_DuplicateKeyRefusedOnDeclaredName(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha", "wf-other"})

	text, isErr := callToolJSON(t, srv, raw,
		`{"workflow":"wf-other","workflow":"wf-alpha","prompt":"p"}`)
	require.True(t, isErr, "a repeated workflow key was admitted: %s", text)
	assert.Contains(t, text, `"workflow"`)
	assert.Equal(t, 0, taskRepo.CallCount.Create, "no task may be created")
}

func TestCompanionArgs_DuplicateKeyRefusedInsideANestedObject(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	text, isErr := callToolJSON(t, srv, raw,
		`{"workflow":"wf-alpha","prompt":"p","inputs":{"since":"2026-01-01","since":"2020-01-01"}}`)
	require.True(t, isErr, "a repeated key inside inputs was admitted: %s", text)
	assert.Contains(t, text, `"since"`)
	assert.Equal(t, 0, taskRepo.CallCount.Create, "no task may be created")
}

// The same name at different levels is not a repeat.
func TestCompanionArgs_SameNameAtDifferentLevelsIsAccepted(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})

	text, isErr := callToolJSON(t, srv, raw,
		`{"workflow":"wf-alpha","prompt":"p","inputs":{"workflow":"x","prompt":"y"}}`)
	require.False(t, isErr, "names repeated at different levels were refused: %s", text)
	assert.Equal(t, 1, taskRepo.CallCount.Create)
}

func TestDuplicateArgKey_Cases(t *testing.T) {
	for _, c := range []struct {
		in   string
		dup  string
		want bool
	}{
		{``, "", false},
		{`{}`, "", false},
		{`{"a":1,"b":2}`, "", false},
		{`{"a":1,"a":2}`, "a", true},
		{`{"a":{"x":1},"b":{"x":2}}`, "", false},                // same key in sibling objects
		{`{"a":[{"x":1},{"x":2}]}`, "", false},                  // same key in array elements
		{`{"a":["x","x"],"x":1}`, "", false},                    // strings in an array are values, not keys
		{`{"a":{"x":1,"x":2}}`, "x", true},                      // nested repeat
		{`{"a":[{"x":1,"x":2}]}`, "x", true},                    // repeat inside an array element
		{`{"a":"b","c":{"d":[1,{"e":null}]},"a":3}`, "a", true}, // repeat after nesting
		{`{"a":"a","b":1}`, "", false},
		{`{"a":1,"\u0061":2}`, "a", true}, // an escaped spelling is the same key to the decoder
		{`not json`, "", false},           // left to the strict decode
	} {
		dup, ok := duplicateArgKey(json.RawMessage(c.in))
		assert.Equalf(t, c.want, ok, "%s", c.in)
		assert.Equalf(t, c.dup, dup, "%s", c.in)
	}
}

// Keys that differ only in case are one argument to the struct decode, which
// matches field names case-insensitively (Unicode simple folding, so the
// Kelvin sign U+212A folds to k): {"Workflow":"a","workflow":"b"} ran "b"
// while the audit showed "a" first (second security review, 2026-10-14).
func TestCompanionArgs_CaseVariantKeysRefused(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha", "wf-other"})

	text, isErr := callToolJSON(t, srv, raw, `{"Workflow":"wf-other","workflow":"wf-alpha","prompt":"p"}`)
	require.True(t, isErr, "case-variant workflow keys were admitted: %s", text)
	assert.Contains(t, text, "more than once")
	assert.Equal(t, 0, taskRepo.CallCount.Create, "no task may be created")
}

func TestDuplicateArgKey_FoldsLikeTheDecoder(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{`{"Workflow":1,"workflow":2}`, true},
		{`{"WORKFLOW":1,"workflow":2}`, true},
		{"{\"Key\":1,\"key\":2}", true}, // Kelvin sign folds to k
		{`{"a":1,"b":2}`, false},
	} {
		_, ok := duplicateArgKey(json.RawMessage(c.in))
		assert.Equalf(t, c.want, ok, "%s", c.in)
	}
}

// The agent admin verbs are dispatched in an earlier branch than the other
// tools, so the repeated-key refusal must run before that branch too (third
// security review, 2026-10-14): create_project / remove / add_api /
// request_credential are the calls whose audit matters most.
func TestCompanionArgs_RepeatedKeyRefusedOnAgentAdminVerb(t *testing.T) {
	srv, keys, _ := newCompanionMCPServer(t)
	fake := &fakeAgentAdmin{}
	srv.agentAdmin, srv.agentAdminEnabled = fake, func() bool { return true }
	agentRaw, err := apikey.Generate("hermes--home")
	require.NoError(t, err)
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{
		ID: "akey-agent-dup", ProjectID: "hermes--home", Name: "hermes", KeyHash: apikey.Hash(agentRaw),
		KeyPrefix: apikey.DisplayPrefix(agentRaw), ClientKind: "hermes", CreatedAt: time.Now().UTC(),
		AgentAdmin: true, AgentNamespace: "hermes",
	}))

	for _, args := range []string{
		`{"slug":"good","purpose":"p","slug":"other"}`,
		`{"Slug":"good","purpose":"p","slug":"other"}`,
	} {
		rec := httptest.NewRecorder()
		srv.CompanionMCPHandler(rec, withCompanionBearer(mcpRequest(t, "tools/call",
			map[string]any{"name": agentadmin.VerbCreateProject, "arguments": json.RawMessage(args)}), agentRaw))
		text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
		require.Truef(t, isErr, "%s was admitted: %s", args, text)
		assert.Contains(t, text, "more than once")
	}
	assert.Empty(t, fake.calls, "the admin verb must not run")
}
