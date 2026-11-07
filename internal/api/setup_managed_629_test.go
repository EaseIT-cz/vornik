package api

// GitHub #629 (operations-api-contract-design.md §6.8): an owner who signs up
// with "my own key later" has no provided model, so the control plane sends
// setup/managed without `models`. Found at: integration (vornik-cloud's client
// against the published contract, EaseIT-cz/vornik-cloud#16), so these run
// through NewRouter on a real config file, like the #582 tests.

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/opsapi"
)

func identityOnlyBody629(subject string) map[string]any {
	b := managedSetupBody582(subject)
	delete(b, "models")
	return b
}

// Without models the call binds the owner and writes no model, no key, and
// probes no gateway; its repeat changes nothing.
func TestSetupManaged629_BindsWithoutModels(t *testing.T) {
	r := newSetupRig582(t, nil)
	code, out := r.post(t, identityOnlyBody629("owner-629"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, true, out["changed"])
	require.Equal(t, true, out["restart_required"])

	cfg, err := config.LoadFromPath(r.path)
	require.NoError(t, err, "the written config must load")
	require.Equal(t, "https://vornik.cloud", cfg.Auth.Providers.OIDC.Issuer)
	require.Contains(t, cfg.Auth.BootstrapAdmins, "vornikcloud:owner-629")
	require.Empty(t, cfg.Chat.Endpoint, "no provided model: no chat endpoint")
	require.Empty(t, cfg.AgentAdmin.ModelIDs(), "no provided model: no catalogue")
	raw, _ := os.ReadFile(r.path)
	require.NotContains(t, string(raw), "VORNIK_CHAT_API_KEY")
	_, err = os.Stat(filepath.Join(r.secrets, "chat.env"))
	require.True(t, os.IsNotExist(err), "no virtual key is written")
	require.Zero(t, r.lister.calls, "no gateway is probed without models")

	before, _ := os.ReadFile(r.path)
	code, out = r.post(t, identityOnlyBody629("owner-629"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, false, out["changed"])
	after, _ := os.ReadFile(r.path)
	require.Equal(t, string(before), string(after))
}

// A later call with models adds them; a call without models after that
// leaves them in place.
func TestSetupManaged629_ModelsAddedLaterAndKept(t *testing.T) {
	r := newSetupRig582(t, nil)
	code, out := r.post(t, identityOnlyBody629("owner-629"))
	require.Equal(t, http.StatusOK, code, "%v", out)

	code, out = r.post(t, managedSetupBody582("owner-629"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, true, out["changed"], "the owner moves to the provided models")
	cfg, err := config.LoadFromPath(r.path)
	require.NoError(t, err)
	require.Equal(t, "https://gateway.internal/v1", cfg.Chat.Endpoint)
	require.Equal(t, []string{"open-a", "open-b"}, cfg.AgentAdmin.ModelIDs())

	before, _ := os.ReadFile(r.path)
	code, out = r.post(t, identityOnlyBody629("owner-629"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, false, out["changed"], "a call without models never removes them")
	after, _ := os.ReadFile(r.path)
	require.Equal(t, string(before), string(after))
	sec, err := os.ReadFile(filepath.Join(r.secrets, "chat.env"))
	require.NoError(t, err)
	require.Contains(t, string(sec), "vk-582-secret", "the virtual key stays")
}

// An empty models object is refused: leaving the field out is the one way to
// say "no provided models".
func TestSetupManaged629_EmptyModelsRefused(t *testing.T) {
	r := newSetupRig582(t, nil)
	b := identityOnlyBody629("owner-629")
	b["models"] = map[string]any{}
	before, _ := os.ReadFile(r.path)
	code, out := r.post(t, b)
	require.Equal(t, http.StatusBadRequest, code, "%v", out)
	require.Equal(t, "BAD_BODY", out["error"].(map[string]any)["code"])
	require.Contains(t, out["error"].(map[string]any)["message"], "leave models out", "the message teaches the one way to send none")
	after, _ := os.ReadFile(r.path)
	require.Equal(t, string(before), string(after))
}

// The owner checks run without models too.
func TestSetupManaged629_OwnerBoundWithoutModels(t *testing.T) {
	r := newSetupRig582(t, nil)
	code, _ := r.post(t, identityOnlyBody629("owner-629"))
	require.Equal(t, http.StatusOK, code)
	code, out := r.post(t, identityOnlyBody629("someone-else"))
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "OWNER_BOUND", out["error"].(map[string]any)["code"])
}

// The contract says models is optional, at 1.3.0.
func TestSetupManaged629_ContractModelsOptional(t *testing.T) {
	raw, ok := opsapi.Document("v1")
	require.True(t, ok)
	doc := string(raw)
	require.Contains(t, doc, "  version: 1.3.0")
	_, rest, ok := strings.Cut(doc, "/api/v1/setup/managed:")
	require.True(t, ok)
	section, _, ok := strings.Cut(rest, "responses:")
	require.True(t, ok)
	require.NotContains(t, section, "required: [identity, models]")
	require.Contains(t, section, "required: [identity]")
}

// Without models the issuer is still probed before an identity is bound, and
// an unavailable one writes nothing (review 16bb F2, F3).
func TestSetupManaged629_IssuerProbedWithoutModels(t *testing.T) {
	r := newSetupRig582(t, errors.New("discovery down"))
	before, _ := os.ReadFile(r.path)
	code, out := r.post(t, identityOnlyBody629("owner-629"))
	require.Equal(t, http.StatusBadGateway, code, "%v", out)
	require.Equal(t, "ISSUER_UNAVAILABLE", out["error"].(map[string]any)["code"])
	after, _ := os.ReadFile(r.path)
	require.Equal(t, string(before), string(after))
}

// After the owner configures their own chat provider, a model-less retry
// leaves it byte for byte (review 16bb F4).
func TestSetupManaged629_OwnChatProviderKept(t *testing.T) {
	r := newSetupRig582(t, nil)
	code, _ := r.post(t, identityOnlyBody629("owner-629"))
	require.Equal(t, http.StatusOK, code)
	raw, _ := os.ReadFile(r.path)
	own := strings.Replace(string(raw), "chat:\n", "chat:\n  enabled: true\n  provider: openai\n  endpoint: https://api.example.test/v1\n  model: own-model\n", 1)
	if own == string(raw) {
		own = string(raw) + "chat:\n  enabled: true\n  provider: openai\n  endpoint: https://api.example.test/v1\n  model: own-model\n"
	}
	require.NoError(t, os.WriteFile(r.path, []byte(own), 0o600))
	code, out := r.post(t, identityOnlyBody629("owner-629"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, false, out["changed"])
	after, _ := os.ReadFile(r.path)
	require.Equal(t, own, string(after), "the owner's own chat provider is not touched")
}

// A with-models call whose virtual key never landed (the daemon stopped after
// the config write): a later call without models does not repair it, as
// §6.8 states; the control plane resends with models (review d4fe S1).
func TestSetupManaged629_InterruptedKeyNotRepairedWithoutModels(t *testing.T) {
	r := newSetupRig582(t, nil)
	code, _ := r.post(t, managedSetupBody582("owner-629"))
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, os.Remove(filepath.Join(r.secrets, "chat.env")))
	code, out := r.post(t, identityOnlyBody629("owner-629"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, false, out["changed"])
	_, err := os.Stat(filepath.Join(r.secrets, "chat.env"))
	require.True(t, os.IsNotExist(err))
	code, out = r.post(t, managedSetupBody582("owner-629"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, true, out["changed"], "resending with models writes the key again")
	_, err = os.Stat(filepath.Join(r.secrets, "chat.env"))
	require.NoError(t, err)
}
