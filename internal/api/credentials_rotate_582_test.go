package api

// GitHub #582 P4 (operations-api-contract-design.md §6.2 P4, §6.3 F3, F5): the
// instance_admin key and the scrape token are replaced through a two-slot
// overlap, the new value sealed to the caller's X-Wing key. Found at: design
// review, observed at integration (the control plane's HTTP call), so these
// run through NewRouter with a real config file on disk; "after the restart"
// is a second router built from the file the operation wrote.

import (
	"bytes"
	"crypto/hpke"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
)

const (
	adminKey582  = "sk-instance-admin-582-aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	scrapeTok582 = "scrape-582-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// managedYAML582 is a managed instance's config, as provision writes it.
func managedYAML582() string {
	return `api:
  auth_enabled: true
  api_keys:
    - ` + adminKey582 + `
  rate_limit:
    per_ip:
      rps: 20
      burst: 40
admin:
  enabled: true
  allowed_keys:
    - ` + adminKey582 + `
server:
  real_ip:
    enabled: true
    trusted_proxies: ["10.0.1.0/24"]
metrics:
  require_admin: true
  scrape_token: ` + scrapeTok582 + `
hosting:
  mode: managed
  operations_address: 10.0.0.5:9443
  model_hosts: [gateway.internal]
  instance_id: i582
`
}

type rig582 struct {
	path string
	hs   *httptest.Server
	priv hpke.PrivateKey
	pub  []byte
}

// newRig582 serves the router built from the config file at path, as a
// daemon started on that file would.
func newRig582(t *testing.T, path string) *rig582 {
	t.Helper()
	cfg, err := config.LoadFromPath(path)
	require.NoError(t, err)
	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithConfig(cfg),
		WithSetupConfigPath(path),
		WithAdminClassKeys(cfg.Admin),
	)
	hs := httptest.NewServer(NewRouter(srv, cfg).Handler())
	t.Cleanup(hs.Close)
	priv, err := hpke.MLKEM768X25519().GenerateKey()
	require.NoError(t, err)
	return &rig582{path: path, hs: hs, priv: priv, pub: priv.PublicKey().Bytes()}
}

func writeConfig582(t *testing.T, body string) string {
	t.Helper()
	require.NoError(t, config.ValidateBytes([]byte(body)), "the fixture must be a valid managed config")
	p := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func (r *rig582) call(t *testing.T, key string, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, r.hs.URL+"/api/v1/operations/credentials/rotate", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// open unseals the answer's new value with the rig's private key.
func (r *rig582) open(t *testing.T, out map[string]any, credential string) string {
	t.Helper()
	sealed, _ := out["sealed"].(string)
	ct, err := base64.StdEncoding.DecodeString(sealed)
	require.NoError(t, err)
	pt, err := hpke.Open(r.priv, hpke.HKDFSHA256(), hpke.AES256GCM(), []byte(rotateInfo582("i582", credential)), ct)
	require.NoError(t, err, "the new value must open with the recipient's private key")
	return string(pt)
}

func rotateInfo582(instanceID, credential string) string {
	return credentialRotateInfoPrefix + instanceID + "/" + credential
}

func authenticates582(t *testing.T, path, key string) int {
	t.Helper()
	r := newRig582(t, path)
	req, _ := http.NewRequest(http.MethodGet, r.hs.URL+"/api/v1/daemon/inflight", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestRotate582_InstanceAdminOverlapThenRetire(t *testing.T) {
	path := writeConfig582(t, managedYAML582())
	r := newRig582(t, path)

	code, out := r.call(t, adminKey582, map[string]any{"credential": "instance_admin", "mode": "rotate", "recipient": base64.StdEncoding.EncodeToString(r.pub)})
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, true, out["restart_required"])
	newKey := r.open(t, out, "instance_admin")
	require.NotEqual(t, adminKey582, newKey)
	// The sealed answer is bound to this instance and this credential
	// (review 4d7e F1): it opens with no other info.
	ct, _ := base64.StdEncoding.DecodeString(out["sealed"].(string))
	for _, info := range []string{rotateInfo582("i582", "scrape_token"), rotateInfo582("other", "instance_admin"), "vornik-provision/1/i582"} {
		_, err := hpke.Open(r.priv, hpke.HKDFSHA256(), hpke.AES256GCM(), []byte(info), ct)
		require.Error(t, err, "the answer opened with info %q", info)
	}
	require.False(t, strings.Contains(mustJSON582(out), newKey), "the new key must not appear in clear")

	// After the restart both keys authenticate.
	require.Equal(t, http.StatusOK, authenticates582(t, path, adminKey582))
	require.Equal(t, http.StatusOK, authenticates582(t, path, newKey))

	// A second rotate before the retire is refused: the previous slot is full.
	r2 := newRig582(t, path)
	code, out = r2.call(t, newKey, map[string]any{"credential": "instance_admin", "mode": "rotate", "recipient": base64.StdEncoding.EncodeToString(r2.pub)})
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "RETIRE_FIRST", out["error"].(map[string]any)["code"])

	// The previous key may not operate the credentials (a stolen old key
	// cannot retire the new one).
	code, _ = r2.call(t, adminKey582, map[string]any{"credential": "instance_admin", "mode": "retire"})
	require.Equal(t, http.StatusForbidden, code)

	// The current key retires the previous one.
	code, out = r2.call(t, newKey, map[string]any{"credential": "instance_admin", "mode": "retire"})
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, http.StatusUnauthorized, authenticates582(t, path, adminKey582), "after the retire the old key must not authenticate")
	require.Equal(t, http.StatusOK, authenticates582(t, path, newKey))

	// Nothing left to retire.
	r3 := newRig582(t, path)
	code, out = r3.call(t, newKey, map[string]any{"credential": "instance_admin", "mode": "retire"})
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "NOTHING_TO_RETIRE", out["error"].(map[string]any)["code"])
}

func TestRotate582_ReplaceRemovesTheOldAtOnce(t *testing.T) {
	path := writeConfig582(t, managedYAML582())
	r := newRig582(t, path)
	code, out := r.call(t, adminKey582, map[string]any{"credential": "instance_admin", "mode": "replace", "recipient": base64.StdEncoding.EncodeToString(r.pub)})
	require.Equal(t, http.StatusOK, code, "%v", out)
	newKey := r.open(t, out, "instance_admin")
	require.Equal(t, http.StatusUnauthorized, authenticates582(t, path, adminKey582))
	require.Equal(t, http.StatusOK, authenticates582(t, path, newKey))
}

func TestRotate582_ScrapeToken(t *testing.T) {
	path := writeConfig582(t, managedYAML582())
	r := newRig582(t, path)
	code, out := r.call(t, adminKey582, map[string]any{"credential": "scrape_token", "mode": "rotate", "recipient": base64.StdEncoding.EncodeToString(r.pub)})
	require.Equal(t, http.StatusOK, code, "%v", out)
	newTok := r.open(t, out, "scrape_token")
	cfg, err := config.LoadFromPath(path)
	require.NoError(t, err)
	require.Equal(t, newTok, cfg.Metrics.ScrapeToken)
	require.Equal(t, scrapeTok582, cfg.Metrics.PreviousScrapeToken)

	// /metrics accepts both tokens during the overlap.
	r2 := newRig582(t, path)
	for _, tok := range []string{scrapeTok582, newTok} {
		req, _ := http.NewRequest(http.MethodGet, r2.hs.URL+"/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.NotEqual(t, http.StatusUnauthorized, resp.StatusCode, "token %d of 2 refused by /metrics", len(tok))
	}

	code, _ = r2.call(t, adminKey582, map[string]any{"credential": "scrape_token", "mode": "retire"})
	require.Equal(t, http.StatusOK, code)
	cfg, err = config.LoadFromPath(path)
	require.NoError(t, err)
	require.Empty(t, cfg.Metrics.PreviousScrapeToken)
}

func TestRotate582_Refusals(t *testing.T) {
	path := writeConfig582(t, managedYAML582())
	r := newRig582(t, path)
	before, _ := os.ReadFile(path)
	cases := []struct {
		name string
		key  string
		body map[string]any
		code int
	}{
		{"no key", "", map[string]any{"credential": "instance_admin", "mode": "rotate", "recipient": base64.StdEncoding.EncodeToString(r.pub)}, http.StatusUnauthorized},
		{"the scrape token is not an operations key", scrapeTok582, map[string]any{"credential": "scrape_token", "mode": "rotate", "recipient": base64.StdEncoding.EncodeToString(r.pub)}, http.StatusUnauthorized},
		{"a malformed recipient", adminKey582, map[string]any{"credential": "instance_admin", "mode": "rotate", "recipient": base64.StdEncoding.EncodeToString([]byte("not a key"))}, http.StatusBadRequest},
		{"no recipient", adminKey582, map[string]any{"credential": "instance_admin", "mode": "rotate"}, http.StatusBadRequest},
		{"an unknown credential", adminKey582, map[string]any{"credential": "db_password", "mode": "rotate", "recipient": base64.StdEncoding.EncodeToString(r.pub)}, http.StatusBadRequest},
		{"an unknown mode", adminKey582, map[string]any{"credential": "instance_admin", "mode": "swap", "recipient": base64.StdEncoding.EncodeToString(r.pub)}, http.StatusBadRequest},
	}
	for _, c := range cases {
		code, _ := r.call(t, c.key, c.body)
		require.Equal(t, c.code, code, c.name)
	}
	after, _ := os.ReadFile(path)
	require.Equal(t, string(before), string(after), "a refused call must write nothing")
}

func mustJSON582(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRotate582_SelfModeRefuses(t *testing.T) {
	srv := NewServer(WithLogger(zerolog.Nop()), WithConfig(&config.Config{}), WithSetupConfigPath("/nonexistent"))
	rr := httptest.NewRecorder()
	srv.RotateOperationsCredential(rr, httptest.NewRequest(http.MethodPost, "/api/v1/operations/credentials/rotate", strings.NewReader(`{}`)))
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Contains(t, rr.Body.String(), "NOT_MANAGED")
}

// Commit security review (2026-10-29), finding 1: between a retire and the
// restart, the retired key still authenticates against the boot-time key
// set. It must not operate the credentials: the file, read under the lock,
// decides which keys are current.
func TestRotate582_ARetiredKeyCannotOperateBeforeTheRestart(t *testing.T) {
	path := writeConfig582(t, managedYAML582())
	r := newRig582(t, path)
	code, out := r.call(t, adminKey582, map[string]any{"credential": "instance_admin", "mode": "rotate", "recipient": base64.StdEncoding.EncodeToString(r.pub)})
	require.Equal(t, http.StatusOK, code, "%v", out)
	newKey := r.open(t, out, "instance_admin")
	// Restart: both keys authenticate; retire the old one with the new.
	r2 := newRig582(t, path)
	code, _ = r2.call(t, newKey, map[string]any{"credential": "instance_admin", "mode": "retire"})
	require.Equal(t, http.StatusOK, code)
	before, _ := os.ReadFile(path)
	// No restart yet: r2's boot-time set still accepts the old key.
	for _, mode := range []string{"replace", "rotate"} {
		code, _ = r2.call(t, adminKey582, map[string]any{"credential": "instance_admin", "mode": mode, "recipient": base64.StdEncoding.EncodeToString(r2.pub)})
		require.Equal(t, http.StatusForbidden, code, "%s with the retired key", mode)
		code, _ = r2.call(t, adminKey582, map[string]any{"credential": "scrape_token", "mode": mode, "recipient": base64.StdEncoding.EncodeToString(r2.pub)})
		require.Equal(t, http.StatusForbidden, code, "scrape token %s with the retired key", mode)
	}
	after, _ := os.ReadFile(path)
	require.Equal(t, string(before), string(after), "a refused call must write nothing")
}

// Finding 2: a key the file holds only as an ${ENV} reference cannot be
// removed by a literal edit; the operation refuses rather than report a
// retirement that did not happen.
func TestRotate582_AnEnvReferencedKeyIsRefused(t *testing.T) {
	t.Setenv("VORNIK_ADMIN_582", adminKey582)
	body := strings.ReplaceAll(managedYAML582(), "    - "+adminKey582+"\n", "    - ${VORNIK_ADMIN_582}\n")
	require.NotContains(t, body, adminKey582)
	path := writeConfig582(t, body)
	r := newRig582(t, path)
	before, _ := os.ReadFile(path)
	code, _ := r.call(t, adminKey582, map[string]any{"credential": "instance_admin", "mode": "replace", "recipient": base64.StdEncoding.EncodeToString(r.pub)})
	require.Equal(t, http.StatusForbidden, code, "a key not literally in the file must not operate the credentials")
	after, _ := os.ReadFile(path)
	require.Equal(t, string(before), string(after))
}
