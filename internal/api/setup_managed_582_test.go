package api

// GitHub #582 P2 (operations-api-contract-design.md §6.2 P2, §6.3 F4, F8): one
// idempotent setup operation binds the customer, its owner as the OpenID
// provider's bootstrap admin and its provided models. Found at: design review,
// observed at integration (the control plane's HTTP call, retried when its
// answer was lost), so these run through NewRouter on a real config file.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
)

// gatewayLister582 lists the gateway's models for the managed setup probe.
type gatewayLister582 struct {
	models []string
	err    error
	calls  int
}

func (g *gatewayLister582) Validate(context.Context, onboarding.ChatConfigProposal) onboarding.ChatValidationResult {
	panic("managed setup must not ping a completion: it spends the customer's allowance")
}

func (g *gatewayLister582) ListModels(context.Context, string, string) ([]chat.ModelInfo, error) {
	g.calls++
	if g.err != nil {
		return nil, g.err
	}
	out := make([]chat.ModelInfo, 0, len(g.models))
	for _, m := range g.models {
		out = append(out, chat.ModelInfo{ID: m})
	}
	return out, nil
}

type rigSetup582 struct {
	path, secrets string
	hs            *httptest.Server
	lister        *gatewayLister582
}

func newSetupRig582(t *testing.T, issuerErr error) *rigSetup582 {
	t.Helper()
	return newSetupRigOn582(t, issuerErr, strings.Replace(managedYAML582(), "server:\n", "server:\n  public_base_url: https://acme.vornik.cloud\n", 1))
}

// newSetupRigOn582 serves managed setup on a config file holding body.
func newSetupRigOn582(t *testing.T, issuerErr error, body string) *rigSetup582 {
	t.Helper()
	path := writeConfig582(t, body)
	secretsDir := filepath.Join(t.TempDir(), "secrets")
	cfg, err := config.LoadFromPath(path)
	require.NoError(t, err)
	lister := &gatewayLister582{models: []string{"open-a", "open-b"}}
	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithConfig(cfg),
		WithSetupConfigPath(path),
		WithSetupSecretsDir(secretsDir),
		WithSetupValidator(lister),
		WithAdminClassKeys(cfg.Admin),
		WithIssuerProbe(func(context.Context, string) error { return issuerErr }),
	)
	hs := httptest.NewServer(NewRouter(srv, cfg).Handler())
	t.Cleanup(hs.Close)
	return &rigSetup582{path: path, secrets: secretsDir, hs: hs, lister: lister}
}

func managedSetupBody582(subject string) map[string]any {
	return map[string]any{
		"identity": map[string]any{"issuer": "https://vornik.cloud", "client_id": "inst-582", "owner_subject": subject, "owner_display": "Owner"},
		"models": map[string]any{
			"gateway_url": "https://gateway.internal/v1", "virtual_key": "vk-582-secret", "default_model": "open-a",
			"catalogue": []map[string]any{{"id": "open-a", "good_for": "general"}, {"id": "open-b", "good_for": "code"}},
		},
	}
}

func (r *rigSetup582) post(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, r.hs.URL+"/api/v1/setup/managed", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+adminKey582)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestSetupManaged582_BindsThenRepeatsWithoutChange(t *testing.T) {
	r := newSetupRig582(t, nil)
	code, out := r.post(t, managedSetupBody582("owner-sub-1"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, true, out["changed"])
	require.Equal(t, true, out["restart_required"])
	require.False(t, strings.Contains(mustJSON582(out), "vk-582-secret"), "the answer must carry no secret")

	cfg, err := config.LoadFromPath(r.path)
	require.NoError(t, err, "the written config must load")
	require.Equal(t, "https://vornik.cloud", cfg.Auth.Providers.OIDC.Issuer)
	require.Equal(t, "inst-582", cfg.Auth.Providers.OIDC.ClientID)
	require.Contains(t, cfg.Auth.BootstrapAdmins, "vornikcloud:owner-sub-1")
	require.Equal(t, "https://gateway.internal/v1", cfg.Chat.Endpoint)
	require.Equal(t, "open-a", cfg.Chat.Model)
	require.Equal(t, []string{"open-a", "open-b"}, cfg.AgentAdmin.ModelIDs())
	raw, _ := os.ReadFile(r.path)
	require.False(t, strings.Contains(string(raw), "vk-582-secret"), "the virtual key must live in the secrets dir, not config.yaml")
	sec, err := os.ReadFile(filepath.Join(r.secrets, "chat.env"))
	require.NoError(t, err)
	require.Contains(t, string(sec), "vk-582-secret")

	// The control plane retries a step whose answer it lost: nothing changes.
	before, _ := os.ReadFile(r.path)
	code, out = r.post(t, managedSetupBody582("owner-sub-1"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, false, out["changed"])
	after, _ := os.ReadFile(r.path)
	require.Equal(t, string(before), string(after))

	// A new virtual key is a change, written.
	b := managedSetupBody582("owner-sub-1")
	b["models"].(map[string]any)["virtual_key"] = "vk-582-rotated"
	code, out = r.post(t, b)
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, true, out["changed"])
	sec, _ = os.ReadFile(filepath.Join(r.secrets, "chat.env"))
	require.Contains(t, string(sec), "vk-582-rotated")

	// A different owner is not setup.
	before, _ = os.ReadFile(r.path)
	code, out = r.post(t, managedSetupBody582("someone-else"))
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "OWNER_BOUND", out["error"].(map[string]any)["code"])
	after, _ = os.ReadFile(r.path)
	require.Equal(t, string(before), string(after), "OWNER_BOUND must write nothing")
}

func TestSetupManaged582_RefusalsWriteNothing(t *testing.T) {
	cases := []struct {
		name      string
		issuerErr error
		listerErr error
		mut       func(map[string]any)
		code      int
		errCode   string
	}{
		{"a gateway outside hosting.model_hosts", nil, nil, func(b map[string]any) { b["models"].(map[string]any)["gateway_url"] = "https://elsewhere.example/v1" }, http.StatusBadRequest, "GATEWAY_NOT_VOUCHED"},
		{"a plain-http gateway", nil, nil, func(b map[string]any) { b["models"].(map[string]any)["gateway_url"] = "http://gateway.internal/v1" }, http.StatusBadRequest, "GATEWAY_NOT_VOUCHED"},
		{"a catalogue id the gateway does not list", nil, nil, func(b map[string]any) {
			b["models"].(map[string]any)["catalogue"] = []map[string]any{{"id": "open-a"}, {"id": "missing"}}
		}, http.StatusBadRequest, "MODEL_NOT_LISTED"},
		{"a default model outside the catalogue", nil, nil, func(b map[string]any) { b["models"].(map[string]any)["default_model"] = "open-z" }, http.StatusBadRequest, "BAD_BODY"},
		{"an unreachable gateway", nil, errors.New("dial refused"), func(map[string]any) {}, http.StatusBadGateway, "GATEWAY_UNAVAILABLE"},
		{"an issuer without discovery", errors.New("no discovery"), nil, func(map[string]any) {}, http.StatusBadGateway, "ISSUER_UNAVAILABLE"},
		{"a plain-http issuer", nil, nil, func(b map[string]any) { b["identity"].(map[string]any)["issuer"] = "http://vornik.cloud" }, http.StatusBadRequest, "BAD_BODY"},
		{"no subject", nil, nil, func(b map[string]any) { b["identity"].(map[string]any)["owner_subject"] = "" }, http.StatusBadRequest, "BAD_BODY"},
		{"an unknown field", nil, nil, func(b map[string]any) { b["extra"] = true }, http.StatusBadRequest, "BAD_BODY"},
	}
	for _, c := range cases {
		r := newSetupRig582(t, c.issuerErr)
		r.lister.err = c.listerErr
		before, _ := os.ReadFile(r.path)
		b := managedSetupBody582("owner-sub-1")
		c.mut(b)
		code, out := r.post(t, b)
		require.Equal(t, c.code, code, "%s: %v", c.name, out)
		require.Equal(t, c.errCode, out["error"].(map[string]any)["code"], c.name)
		after, _ := os.ReadFile(r.path)
		require.Equal(t, string(before), string(after), "%s: a refusal must write nothing", c.name)
		_, err := os.Stat(filepath.Join(r.secrets, "chat.env"))
		require.True(t, os.IsNotExist(err), "%s: a refusal must write no secret", c.name)
	}
}

func TestSetupManaged582_SelfModeRefuses(t *testing.T) {
	srv := NewServer(WithLogger(zerolog.Nop()), WithConfig(&config.Config{}), WithSetupConfigPath("/nonexistent"))
	rr := httptest.NewRecorder()
	srv.SetupManaged(rr, httptest.NewRequest(http.MethodPost, "/api/v1/setup/managed", strings.NewReader(`{}`)))
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Contains(t, rr.Body.String(), "NOT_MANAGED")
}

// Commit security review (2026-10-29), finding 4: once an owner is bound,
// another issuer or client id is an owner change (a new issuer could mint the
// same subject), refused like another subject.
func TestSetupManaged582_TheIssuerIsBoundWithTheOwner(t *testing.T) {
	r := newSetupRig582(t, nil)
	code, out := r.post(t, managedSetupBody582("owner-sub-1"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	before, _ := os.ReadFile(r.path)
	for _, mut := range []func(map[string]any){
		func(b map[string]any) { b["identity"].(map[string]any)["issuer"] = "https://evil.example" },
		func(b map[string]any) { b["identity"].(map[string]any)["client_id"] = "another-client" },
	} {
		b := managedSetupBody582("owner-sub-1")
		mut(b)
		code, out = r.post(t, b)
		require.Equal(t, http.StatusConflict, code, "%v", out)
		require.Equal(t, "OWNER_BOUND", out["error"].(map[string]any)["code"])
	}
	after, _ := os.ReadFile(r.path)
	require.Equal(t, string(before), string(after))
}

// Finding 3: managed setup takes the current instance_admin key only, never
// a session and never the previous key of a rotation.
func TestSetupManaged582_OnlyTheCurrentInstanceAdminKey(t *testing.T) {
	r := newSetupRig582(t, nil)
	// The previous key of a rotation, written to the file.
	raw, _ := os.ReadFile(r.path)
	other := "sk-other-admin-582-cccccccccccccccccccccccccccc"
	body := strings.Replace(string(raw), "  api_keys:\n    - "+adminKey582+"\n", "  api_keys:\n    - "+adminKey582+"\n    - "+other+"\n", 1)
	body = strings.Replace(body, "  allowed_keys:\n    - "+adminKey582+"\n", "  allowed_keys:\n    - "+adminKey582+"\n    - "+other+"\n", 1)
	body = strings.Replace(body, "  instance_id: i582\n", "  instance_id: i582\n  previous_admin_api_key: "+adminKey582+"\n", 1)
	require.NoError(t, config.ValidateBytes([]byte(body)))
	require.NoError(t, os.WriteFile(r.path, []byte(body), 0o600))
	code, out := r.post(t, managedSetupBody582("owner-sub-1"))
	require.Equal(t, http.StatusForbidden, code, "the previous key must not bind an owner: %v", out)
}
