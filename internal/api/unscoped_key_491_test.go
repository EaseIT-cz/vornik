package api

// GitHub #491 (P1, security; operator decision B7, 2026-10-21): an unscoped
// static key that is not admin-class (an api.api_keys entry not in
// admin.allowed_keys) read the SCRUBBED view of every project's task logs,
// explain, live socket and persisted error text, because #440 and #466 chose
// the view from IsAdminClassRequest alone. The decision is deny until scoped:
// such a key gets 403 on the log, explain and stream routes and no line of
// the error text on the structured reads; a project-scoped key still reads
// its own project's scrubbed view. Found at: review (the #440 lane's open
// decision 2, decided by the operator); an attacker holding such a key
// reaches it on the HTTP route and the socket, so these tests run the real
// router (NewRouter: AuthMiddleware with the configured static keys, a DB
// project key, the project gate) over real SQLite (process-spawn-law design,
// "Amendment 2026-10-21 (#491)").

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/postmortem"
	"vornik.io/vornik/internal/report"
)

// adminClassReq491 is r as an admin-class caller (an admin browser session
// with auth on). Since #491 a request with no principal at all is refused
// the logs, explain and error text (the default arm), so a handler test that
// means the operator says so.
func adminClassReq491(r *http.Request) *http.Request {
	ctx := context.WithValue(r.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, identityKey, &auth.Identity{
		Backend: "session", Projects: []string{"*"},
		Extra: map[string]any{auth.ExtraSessionRole: "admin"},
	})
	return r.WithContext(ctx)
}

// staticKey491 is an api.api_keys entry that is NOT in admin.allowed_keys:
// unscoped (BuildAuthConfig maps every configured static key to all
// projects) and not admin-class.
const staticKey491 = "sk-static-not-admin-491"

type countingDiagnoser491 struct{ calls atomic.Int32 }

func (d *countingDiagnoser491) Diagnose(context.Context, string, bool) (*controlplane.DiagnoseVerdict, string, error) {
	d.calls.Add(1)
	return &controlplane.DiagnoseVerdict{}, "", nil
}

func newHarness491(t *testing.T) (*harness466, *countingDiagnoser491) {
	t.Helper()
	d := &countingDiagnoser491{}
	h := newHarness466With(t, []string{staticKey491}, WithDiagnoser(d))
	h.srv.explainRenderer = postmortem.NewRenderer(h.tasks, nil, nil, nil, nil)
	return h, d
}

func (h *harness466) do(t *testing.T, method, path, bearer, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, h.hs.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		b.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

// refusedRoutes491 are the routes whose body is the log, the explain or the
// diagnose verdict (design K2, first arm).
var refusedRoutes491 = []struct{ method, path, body string }{
	{http.MethodGet, "/api/v1/projects/alpha/tasks/t466/logs", ""},
	{http.MethodGet, "/api/v1/projects/alpha/tasks/t466/explain", ""},
	{http.MethodPost, "/api/v1/projects/alpha/tasks/t466/explain", ""},
	{http.MethodPost, "/api/v1/operator/diagnose", `{"focus":"alpha"}`},
}

func assertNoErrorLine491(t *testing.T, where, text string) {
	t.Helper()
	assertNoSecret466(t, where, text)
	assert.NotContains(t, text, "exit status 1", "%s: a line of the error text reached an unscoped key", where)
	assert.NotContains(t, text, report.WithheldLine, "%s: the scrubbed view reached an unscoped key", where)
}

func TestLogAccess491_UnscopedStaticKeyRefused(t *testing.T) {
	h, diag := newHarness491(t)

	for _, rt := range refusedRoutes491 {
		code, body := h.do(t, rt.method, rt.path, staticKey491, rt.body)
		assert.Equal(t, http.StatusForbidden, code, "%s %s: %s", rt.method, rt.path, body)
		want := "PROJECT_SCOPE_REQUIRED"
		if strings.HasPrefix(rt.path, "/api/v1/operator/") {
			// GitHub #495: the operator-surface gate refuses this key first.
			want = "OPERATOR_CAPABILITY_REQUIRED"
		}
		assert.Contains(t, body, want, "%s %s", rt.method, rt.path)
		assertNoErrorLine491(t, rt.path, body)
	}
	assert.Zero(t, diag.calls.Load(), "diagnose ran for an unscoped non-admin key")

	// The live socket: refused before the upgrade.
	_, resp, err := h.dial(t, staticKey491)
	require.Error(t, err, "the live socket opened for an unscoped non-admin key")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// The structured reads keep working, with no line of the error text.
	for _, p := range restPaths466 {
		code, body := h.get(t, p, staticKey491)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		text := decodedStrings466(t, body)
		assertNoErrorLine491(t, p, text)
		assert.Contains(t, text, UnscopedWithheldText, p)
	}
}

// The decision is taken before the scrubber is consulted: with no scrubber
// the unscoped key still gets 403, not the 503 a scoped key gets.
func TestLogAccess491_NoScrubberStillRefusesUnscoped(t *testing.T) {
	h, _ := newHarness491(t)
	h.srv.liveLogsOnce.Do(func() {})
	h.srv.liveLogsState = &liveLogState{}

	for _, rt := range refusedRoutes491[:3] {
		code, body := h.do(t, rt.method, rt.path, staticKey491, rt.body)
		assert.Equal(t, http.StatusForbidden, code, "%s %s: %s", rt.method, rt.path, body)
	}
	_, resp, err := h.dial(t, staticKey491)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	for _, p := range restPaths466 {
		_, body := h.get(t, p, staticKey491)
		assert.Contains(t, decodedStrings466(t, body), UnscopedWithheldText, p)
	}
}

// A project-scoped key still reads its own project's scrubbed view; the
// admin-class static key keeps the operator view.
func TestLogAccess491_ScopedAndAdminKeysUnchanged(t *testing.T) {
	h, _ := newHarness491(t)

	code, body := h.get(t, "/api/v1/projects/alpha/tasks/t466/logs", h.projectKey)
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, report.WithheldLine)
	assert.Contains(t, body, "exit status 1")
	assertNoSecret466(t, "logs", body)

	code, body = h.get(t, "/api/v1/projects/alpha/tasks/t466/explain", h.projectKey)
	require.Equal(t, http.StatusOK, code, body)
	assertNoSecret466(t, "explain", decodedStrings466(t, body))

	for _, p := range restPaths466 {
		code, body := h.get(t, p, h.projectKey)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		text := decodedStrings466(t, body)
		assert.Contains(t, text, "exit status 1", p)
		assert.NotContains(t, text, UnscopedWithheldText, p)
	}

	code, body = h.get(t, "/api/v1/projects/alpha/tasks/t466/logs", adminKey466)
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, plantedToken440, "the operator view must stay unchanged")
	for _, p := range restPaths466 {
		_, body := h.get(t, p, adminKey466)
		assert.NotContains(t, decodedStrings466(t, body), UnscopedWithheldText, p)
	}

	conn, resp, err := h.dial(t, h.projectKey)
	require.NoError(t, err, "%v", resp)
	_ = conn.CloseNow()
}

// The decision table (design K1). Denied is the default arm: a principal
// with no scope that is neither admin-class nor a session reads nothing.
func TestErrorTextAccess491_DecisionTable(t *testing.T) {
	isAdmin := func(k string) bool { return k == "admin" }
	mk := func(ctx context.Context) *http.Request {
		return (&http.Request{}).WithContext(ctx)
	}
	off := context.WithValue(context.Background(), authEnabledKey, false)
	on := context.WithValue(context.Background(), authEnabledKey, true)
	withKey := func(ctx context.Context, k string) context.Context { return context.WithValue(ctx, apiKeyKey, k) }
	withID := func(ctx context.Context, id *auth.Identity) context.Context {
		return context.WithValue(ctx, identityKey, id)
	}
	cases := []struct {
		name string
		ctx  context.Context
		want ErrorTextAccess
	}{
		{"auth off", off, ErrorTextOperator},
		{"admin-class key", withKey(on, "admin"), ErrorTextOperator},
		{"admin session", withID(on, &auth.Identity{Backend: "session", Extra: map[string]any{auth.ExtraSessionRole: "admin"}}), ErrorTextOperator},
		{"scoped key", context.WithValue(withKey(on, "k"), projectIDKey, []string{"alpha"}), ErrorTextScrubbed},
		{"no-access session", context.WithValue(withID(on, &auth.Identity{Backend: "session"}), projectIDKey, []string{noAccessSentinelProject}), ErrorTextScrubbed},
		{"empty-grant session, no sentinel", withID(on, &auth.Identity{Backend: "session"}), ErrorTextScrubbed},
		{"star-group session", withID(on, &auth.Identity{Backend: "session", Projects: []string{"*"}, Extra: map[string]any{auth.ExtraSessionRole: "user"}}), ErrorTextScrubbed},
		{"unscoped static key", withID(withKey(on, "k"), &auth.Identity{Backend: "static-keys"}), ErrorTextDenied},
		{"unscoped key, no identity", withKey(on, "k"), ErrorTextDenied},
		{"hmac webhook", withID(on, &auth.Identity{Backend: "hmac-webhook"}), ErrorTextDenied},
		{"empty-project DB key", context.WithValue(withKey(on, "k"), projectIDKey, []string{""}), ErrorTextDenied},
		{"unknown backend", withID(on, &auth.Identity{Backend: "future"}), ErrorTextDenied},
		{"nothing at all", on, ErrorTextDenied},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, ErrorTextAccessFor(mk(c.ctx), isAdmin), c.name)
	}
}

// Adversarial review 2026-10-21 (W18-A, finding 1): the control-plane
// proposal ledger stores the diagnose verdict the self-heal worker files on
// its own (rationale = the model's root cause, evidence = the verdict), a
// model's reading of the projects' error text, and served it to any unscoped
// caller through requireOperatorScope. Structured reads: the fields read the
// constant (design K2).
func TestProposals491_UnscopedStaticKeyRefused(t *testing.T) {
	store := sqlite.NewProposalRepository(sqlitetest.Memory(t).DB)
	h := newHarness466With(t, []string{staticKey491}, WithProposalStore(store))
	rationale := "root cause: the step failed with exit status 1"
	code, body := h.do(t, http.MethodPost, "/api/v1/operator/proposals", adminKey466,
		`{"kind":"config","blastRadius":"project","projectId":"alpha","title":"t","diff":"-a\n+b","proposedBy":"selfheal","rationale":"`+rationale+`"}`)
	require.Equal(t, http.StatusCreated, code, body)
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &created), body)
	require.NotEmpty(t, created.ID, body)

	// Since GitHub #495 the operator-surface gate refuses this key outright
	// (403 OPERATOR_CAPABILITY_REQUIRED) before any proposal is read; the
	// per-access mapping stays as the reader-side guard and is pinned on
	// proposalJSONFor in TestProposals491_EveryTextFieldFollowsTheAccessMapping.
	for _, p := range []string{"/api/v1/operator/proposals", "/api/v1/operator/proposals/" + created.ID} {
		code, body := h.get(t, p, staticKey491)
		require.Equal(t, http.StatusForbidden, code, "%s: %s", p, body)
		assert.Contains(t, body, "OPERATOR_CAPABILITY_REQUIRED", p)
		assert.NotContains(t, decodedStrings466(t, body), rationale, p)

		code, body = h.get(t, p, adminKey466)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		assert.Contains(t, body, rationale, "%s: the operator still reads the ledger", p)
	}

	// A write is refused as well, and carries no proposal.
	code, body = h.do(t, http.MethodPost, "/api/v1/operator/proposals/"+created.ID+"/decide", staticKey491,
		`{"decision":"reject","actor":"x"}`)
	require.Equal(t, http.StatusForbidden, code, body)
	assert.NotContains(t, decodedStrings466(t, body), rationale, "decide")
}

// Coordinator security review 2026-10-21 (MEDIUM x2): a diagnose-filed
// proposal's Title is "Diagnose: " + the root cause (diagnose.go), and its
// Diff the rendered change; the first follow-up withheld only rationale and
// evidence, and only for a Denied caller. A Scrubbed caller that reaches the
// ledger (a non-admin ["*"] session: requireOperatorScope admits any
// unscoped caller) read all four raw. Every text field now goes through the
// one per-access mapping, errorTextView.
func TestProposals491_EveryTextFieldFollowsTheAccessMapping(t *testing.T) {
	store := sqlite.NewProposalRepository(sqlitetest.Memory(t).DB)
	h := newHarness466With(t, []string{staticKey491}, WithProposalStore(store))
	title := "Diagnose: provider rejected Bearer " + plantedToken440
	diff := "-timeout: 30\n+token: " + awsKey440 + "\n+timeout: 90"
	payload, _ := json.Marshal(map[string]string{
		"kind": "config", "blastRadius": "project", "projectId": "alpha",
		"title": title, "diff": diff, "proposedBy": "selfheal",
		"rationale": "root cause: Bearer " + plantedToken440, "evidence": `{"root_cause":"` + slackKey440 + `"}`,
	})
	code, body := h.do(t, http.MethodPost, "/api/v1/operator/proposals", adminKey466, string(payload))
	require.Equal(t, http.StatusCreated, code, body)
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &created), body)

	// A non-admin ["*"] session, stamped as the middleware stamps it.
	star := &auth.Identity{Backend: "session", Projects: []string{"*"}, Extra: map[string]any{auth.ExtraSessionRole: "user"}}
	starCtx := stampIdentityContext(context.WithValue(context.Background(), authEnabledKey, true), star, "", AuthConfig{})
	require.Equal(t, ErrorTextScrubbed, ErrorTextAccessFor((&http.Request{}).WithContext(starCtx), nil))

	for _, p := range []string{"/api/v1/operator/proposals", "/api/v1/operator/proposals/" + created.ID} {
		// GitHub #495: both non-admin callers are refused at the gate now,
		// with no line of any field.
		code, body := h.get(t, p, staticKey491)
		assert.Equal(t, http.StatusForbidden, code, "denied %s", p)
		text := body
		assertNoSecret466(t, "denied "+p, text)
		assert.NotContains(t, text, "provider rejected", "denied %s: the title's root cause", p)
		assert.NotContains(t, text, "timeout: 90", "denied %s: the diff", p)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, p, nil).WithContext(starCtx)
		if strings.HasSuffix(p, "/proposals") {
			h.srv.OperatorProposals(rec, req)
		} else {
			h.srv.OperatorProposalItem(rec, req)
		}
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertNoSecret466(t, "star session "+p, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "timeout: 90", "star session %s", p)

		_, body = h.get(t, p, adminKey466)
		assert.Contains(t, decodedStrings466(t, body), plantedToken440, "the operator view is unchanged")
	}

	// The mapping itself stays as the reader-side guard (#495 design O4):
	// every text field follows the caller's view on proposalJSONFor.
	stored, err := store.GetByID(context.Background(), created.ID)
	require.NoError(t, err)
	denied := (&http.Request{}).WithContext(context.WithValue(context.Background(), authEnabledKey, true))
	j := h.srv.proposalJSONFor(denied, stored)
	for _, f := range []string{j.Title, j.Diff, j.Rationale, j.Evidence} {
		assert.Equal(t, UnscopedWithheldText, f, "denied view")
	}
	j = h.srv.proposalJSONFor((&http.Request{}).WithContext(starCtx), stored)
	scrubbed := strings.Join([]string{j.Title, j.Diff, j.Rationale, j.Evidence}, "\n")
	assertNoSecret466(t, "scrubbed view", scrubbed)
	assert.Contains(t, scrubbed, report.WithheldLine)
	assert.Contains(t, scrubbed, "timeout: 90", "scrubbed: a clean diff line is still shown")
}

// Round-2 code review (review-20261021-f0c2, F1/F2): apply and rollback share
// respondApplyOutcome, which answers with the proposal; pin that both answer
// through the access mapping, as decide does, with the constant in place.
func TestProposals491_ApplyAndRollbackResponsesFollowTheMapping(t *testing.T) {
	s := newProposalTestServer(t)
	s.proposalApplier = &fakeApplier{}
	for _, op := range []string{"apply", "rollback"} {
		id := seedApproved(t, s) // title "t"
		rec := httptest.NewRecorder()
		// Auth on, no key, no scope: the Denied arm. Since GitHub #495 the
		// operator-surface gate refuses it before the apply runs, so no
		// proposal is answered at all.
		denied := httptest.NewRequest(http.MethodPost, "/api/v1/operator/proposals/"+id+"/"+op, strings.NewReader(`{"actor":"x","ackDaemon":true}`))
		denied = denied.WithContext(context.WithValue(denied.Context(), authEnabledKey, true))
		s.OperatorProposalItem(rec, denied)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", op, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), `"title"`, op)

		// The operator's apply still answers through the mapping (operator
		// view, unchanged).
		rec = httptest.NewRecorder()
		s.OperatorProposalItem(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals/"+id+"/"+op, `{"actor":"x","ackDaemon":true}`))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", op, rec.Body.String())
		assert.Equal(t, "t", decodeProposal(t, rec.Body.String()).Title, op)

		rec = httptest.NewRecorder()
		s.OperatorProposalItem(rec, adminClassReq491(operatorReq(http.MethodGet, "/api/v1/operator/proposals/"+id, "")))
		assert.Equal(t, "t", decodeProposal(t, rec.Body.String()).Title, "%s: the operator view is unchanged", op)
	}
}
