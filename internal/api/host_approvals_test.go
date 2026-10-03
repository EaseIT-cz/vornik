package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// Hermes approval transport design §4.3: POST /api/v1/agent/host-approvals
// files a Hermes approval request (idempotent), GET .../{request_id}?wait=N
// long-polls its state. Both take only an agent admin key; the namespace
// comes from the key, never the body.

func newHostApprovalServer(t *testing.T) (*Server, *memAPIKeyRepo, *approverdevice.Service) {
	t.Helper()
	db := sqlitetest.File(t, "ha.db")
	svc := approverdevice.New(sqlite.NewApproverDeviceRepository(db.DB))
	keys := &memAPIKeyRepo{}
	srv := &Server{logger: zerolog.Nop(), apiKeyRepo: keys}
	WithHostApprovals(svc)(srv)
	return srv, keys, svc
}

func seedAgentAdminKey(t *testing.T, repo *memAPIKeyRepo, ns string) string {
	t.Helper()
	raw, err := apikey.Generate(ns + "--home")
	require.NoError(t, err)
	require.NoError(t, repo.Create(context.Background(), &persistence.APIKey{ID: "akey-admin-" + ns, ProjectID: ns + "--home",
		Name: "agent admin", KeyHash: apikey.Hash(raw), KeyPrefix: apikey.DisplayPrefix(raw), ClientKind: "hermes",
		AgentAdmin: true, AgentNamespace: ns, CreatedAt: time.Now().UTC()}))
	return raw
}

func hostBody(id string) []byte {
	b, _ := json.Marshal(map[string]any{"request_id": id, "digest": strings.Repeat("d", 64), "command": "rm -rf /tmp/x",
		"description": "recursive delete", "pattern_key": "rm", "pattern_keys": []string{"rm"}, "surface": "cli",
		"timeout_seconds": 300, "allowed_choices": []string{"once", "session", "always", "deny"},
		// A namespace in the body is ignored: the key decides.
		"namespace": "other"})
	return b
}

func hostDo(srv *Server, method, path, raw string, body []byte) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if raw != "" {
		req = withCompanionBearer(req, raw)
	}
	rec := httptest.NewRecorder()
	srv.hostApprovalsRouter(rec, req)
	return rec
}

func TestHostApprovals_FileIsIdempotentAndBoundToTheKeysNamespace(t *testing.T) {
	srv, keys, _ := newHostApprovalServer(t)
	hermes := seedAgentAdminKey(t, keys, "hermes")
	other := seedAgentAdminKey(t, keys, "other")

	rec := hostDo(srv, http.MethodPost, "/api/v1/agent/host-approvals", hermes, hostBody("ab01"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var st struct {
		Status   string    `json:"status"`
		Choice   string    `json:"choice"`
		Deadline time.Time `json:"deadline"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	require.Equal(t, "pending", st.Status)
	// The same filing again answers the same state.
	rec = hostDo(srv, http.MethodPost, "/api/v1/agent/host-approvals", hermes, hostBody("ab01"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The namespace came from the key: hermes sees it, other does not.
	rec = hostDo(srv, http.MethodGet, "/api/v1/agent/host-approvals/ab01", hermes, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = hostDo(srv, http.MethodGet, "/api/v1/agent/host-approvals/ab01", other, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "another namespace's request must be 404, like a missing one")
	rec = hostDo(srv, http.MethodGet, "/api/v1/agent/host-approvals/ffff", hermes, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHostApprovals_ConflictBusyInvalid(t *testing.T) {
	srv, keys, _ := newHostApprovalServer(t)
	hermes := seedAgentAdminKey(t, keys, "hermes")
	require.Equal(t, http.StatusOK, hostDo(srv, http.MethodPost, "/api/v1/agent/host-approvals", hermes, hostBody("ab01")).Code)
	var changed map[string]any
	require.NoError(t, json.Unmarshal(hostBody("ab01"), &changed))
	changed["digest"] = strings.Repeat("e", 64)
	raw, _ := json.Marshal(changed)
	rec := hostDo(srv, http.MethodPost, "/api/v1/agent/host-approvals", hermes, raw)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"conflict"`)

	for _, id := range []string{"ab02", "ab03"} {
		require.Equal(t, http.StatusOK, hostDo(srv, http.MethodPost, "/api/v1/agent/host-approvals", hermes, hostBody(id)).Code)
	}
	rec = hostDo(srv, http.MethodPost, "/api/v1/agent/host-approvals", hermes, hostBody("ab04"))
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"busy"`)

	rec = hostDo(srv, http.MethodPost, "/api/v1/agent/host-approvals", hermes, []byte(`{"request_id":"zz"}`))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// §6: a non-admin companion key is 403 on both routes; so is an operator
// key (a static key no DB row has) and a request with no key (a web session
// carries none).
func TestHostApprovals_OnlyAnAgentAdminKey(t *testing.T) {
	srv, keys, _ := newHostApprovalServer(t)
	plain, _ := seedCompanionKey(t, keys, "alpha", nil)
	for name, raw := range map[string]string{"companion key": plain, "operator key": "sk-operator-static", "no key": ""} {
		for _, rt := range []struct{ method, path string }{
			{http.MethodPost, "/api/v1/agent/host-approvals"}, {http.MethodGet, "/api/v1/agent/host-approvals/ab01"},
		} {
			var body []byte
			if rt.method == http.MethodPost {
				body = hostBody("ab01")
			}
			rec := hostDo(srv, rt.method, rt.path, raw, body)
			require.Equalf(t, http.StatusForbidden, rec.Code, "%s %s with %s", rt.method, rt.path, name)
		}
	}
}

// §4.3: the GET long-polls until the state changes or wait elapses (at
// most 25 s).
func TestHostApprovals_GetWaitsForTheAnswer(t *testing.T) {
	srv, keys, svc := newHostApprovalServer(t)
	hermes := seedAgentAdminKey(t, keys, "hermes")
	require.Equal(t, http.StatusOK, hostDo(srv, http.MethodPost, "/api/v1/agent/host-approvals", hermes, hostBody("ab01")).Code)
	ctx := context.Background()
	code, _, _ := svc.StartPairing(ctx, "Phone")
	red, err := svc.Redeem(ctx, code, "10.0.0.1")
	require.NoError(t, err)
	id := approverdevice.HostActionID("hermes", "ab01")
	row, err := svc.Request(ctx, id)
	require.NoError(t, err)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = svc.DecideChoice(ctx, red.Device, id, row.RenderedSHA256, "session")
	}()
	start := time.Now()
	rec := hostDo(srv, http.MethodGet, "/api/v1/agent/host-approvals/ab01?wait=5", hermes, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"choice":"session"`)
	require.Less(t, time.Since(start), 4*time.Second, "the poll did not return when the answer came")
	require.Equal(t, 25*time.Second, hostApprovalWait("600"))
	require.Equal(t, time.Duration(0), hostApprovalWait("-3"))
}

// Review 20261003-c8fd item 3 (design §4.1, §4.4: "no command text in a
// push"): across every filing outcome (filed, refiled, conflict, busy,
// invalid, a store failure), neither a push nor a log line carries the
// command. It holds by construction; this pins it.
func TestHostApprovals_NoCommandTextInPushOrLog(t *testing.T) {
	const canary = "COMMANDCANARY-rm-rf-7f3c"
	db := sqlitetest.File(t, "ha.db")
	var pushes []string
	svc := approverdevice.New(sqlite.NewApproverDeviceRepository(db.DB),
		approverdevice.WithNotifier(func(_ context.Context, subject, body string) { pushes = append(pushes, subject+"\n"+body) }))
	var logs bytes.Buffer
	keys := &memAPIKeyRepo{}
	srv := &Server{logger: zerolog.New(&logs).Level(zerolog.DebugLevel), apiKeyRepo: keys}
	WithHostApprovals(svc)(srv)
	key := seedAgentAdminKey(t, keys, "hermes")
	body := func(id, digest string) []byte {
		b, _ := json.Marshal(map[string]any{"request_id": id, "digest": digest, "command": "rm -rf /tmp/" + canary,
			"description": "recursive delete", "pattern_key": "rm", "surface": "cli", "timeout_seconds": 300,
			"allowed_choices": []string{"once", "deny"}})
		return b
	}
	d1, d2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	codes := []int{}
	for _, b := range [][]byte{body("ab01", d1), body("ab01", d1), body("ab01", d2), body("ab02", d1), body("ab03", d1),
		body("ab04", d1), body("zz", d1)} {
		codes = append(codes, hostDo(srv, http.MethodPost, "/api/v1/agent/host-approvals", key, b).Code)
	}
	require.Equal(t, []int{200, 200, 409, 200, 200, 429, 400}, codes)
	// A store failure: the handler logs the error.
	require.NoError(t, db.Close())
	require.Equal(t, http.StatusInternalServerError, hostDo(srv, http.MethodPost, "/api/v1/agent/host-approvals", key, body("ab05", d1)).Code)
	require.NotEmpty(t, pushes)
	require.NotEmpty(t, logs.String(), "the store failure was not logged, so the log was not examined")
	for _, p := range pushes {
		require.NotContains(t, p, canary)
		require.NotContains(t, p, "rm -rf")
	}
	require.NotContains(t, logs.String(), canary)
}

// The admin-key confinement admits the host-approval routes only for an
// agent admin key, by exact shape (Hermes approval transport design §4.3).
func TestIsCompanionAllowedPathFor_HostApprovals(t *testing.T) {
	admin := &persistence.APIKey{ClientKind: "hermes", AgentAdmin: true, AgentNamespace: "hermes"}
	plain := &persistence.APIKey{ClientKind: "claude-code"}
	for _, p := range []string{"/api/v1/agent/host-approvals", "/api/v1/agent/host-approvals/ab01"} {
		require.Truef(t, isCompanionAllowedPathFor(admin, p), "admin key refused %s", p)
		require.Falsef(t, isCompanionAllowedPathFor(plain, p), "a non-admin companion key reached %s", p)
	}
	for _, p := range []string{"/api/v1/agent/host-approvals/", "/api/v1/agent/host-approvals/a/b", "/api/v1/agent/other", "/api/v1/agent/host-approvalsx"} {
		require.Falsef(t, isCompanionAllowedPathFor(admin, p), "admin key reached %s", p)
	}
	require.True(t, isCompanionAllowedPathFor(plain, "/api/v1/mcp/companion"))
}
