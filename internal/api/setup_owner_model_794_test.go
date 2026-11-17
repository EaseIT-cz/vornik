package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/opsapi"
)

// GitHub #794 (security design 18, amendment 2026-11-16, P5): the owner's
// model provider is written as exactly the chat keys setup writes, with the
// key behind its own placeholder, and the agent catalogue reduced to the
// chosen model.
func TestOwnerModelProvider794_ContentWritesTheChatKeys(t *testing.T) {
	in := []byte(`chat:
  enabled: true
  provider: http
  endpoint: https://gateway.example/v1
  model: provided-model
  api_key: ${VORNIK_CHAT_API_KEY}
agent_admin:
  models:
    - id: provided-model
      good_for: everything
    - id: provided-small
      good_for: quick
server:
  address: 127.0.0.1:8080
`)
	out, err := OwnerModelProviderContent(in, "https://api.provider.example/v1", "owner-model")
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("written config does not parse: %v\n%s", err, out)
	}
	if !cfg.Chat.Enabled || cfg.Chat.Provider != "http" || cfg.Chat.Endpoint != "https://api.provider.example/v1" || cfg.Chat.Model != "owner-model" {
		t.Errorf("chat block = %+v", cfg.Chat)
	}
	if !strings.Contains(string(out), "api_key: ${"+OwnerModelAPIKeyName+"}") {
		t.Errorf("chat.api_key does not reference %s:\n%s", OwnerModelAPIKeyName, out)
	}
	if strings.Contains(string(out), "VORNIK_CHAT_API_KEY") {
		t.Errorf("the provided key's placeholder survived, and the environment would shadow the owner's key:\n%s", out)
	}
	if len(cfg.AgentAdmin.Models) != 1 || cfg.AgentAdmin.Models[0].ID != "owner-model" {
		t.Errorf("agent_admin.models = %+v, want only the chosen model", cfg.AgentAdmin.Models)
	}
	if cfg.Server.Address != "127.0.0.1:8080" {
		t.Errorf("an unrelated key changed: server.address = %q", cfg.Server.Address)
	}
}

// Operations-API design §6.12 R2: setup/status reports a pending restart and
// its reasons, names only.
func TestSetupStatus794_ReportsRestartPending(t *testing.T) {
	cfg := &config.Config{}
	for _, tc := range []struct {
		name    string
		reasons []string
		pending bool
	}{
		{"nothing pending", nil, false},
		{"owner saved a provider", []string{"model_provider"}, true},
		{"a reload failed and chat.model changed", []string{"config:chat.model", "reload"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{config: cfg, setupDetector: onboarding.Detector{Config: cfg}}
			reasons := tc.reasons
			WithRestartPendingSource(func() []string { return reasons })(s)
			rec := httptest.NewRecorder()
			s.SetupStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/setup/status", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			var body struct {
				FreshInstall   *bool    `json:"fresh_install"`
				RestartPending *bool    `json:"restart_pending"`
				RestartReasons []string `json:"restart_reasons"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.FreshInstall == nil {
				t.Error("the detector's fields are gone from the response")
			}
			if body.RestartPending == nil || *body.RestartPending != tc.pending {
				t.Errorf("restart_pending = %v, want %v", body.RestartPending, tc.pending)
			}
			want := tc.reasons
			if want == nil {
				want = []string{}
			}
			if !slices.Equal(body.RestartReasons, want) {
				t.Errorf("restart_reasons = %v, want %v", body.RestartReasons, want)
			}
			if !strings.Contains(rec.Body.String(), `"restart_reasons":[`) {
				t.Errorf("restart_reasons is not an array: %s", rec.Body.String())
			}
		})
	}
}

// Without a source wired (an older wiring, tests), setup/status still answers
// restart_pending false and an empty list.
func TestSetupStatus794_NoSourceMeansNotPending(t *testing.T) {
	cfg := &config.Config{}
	s := &Server{config: cfg, setupDetector: onboarding.Detector{Config: cfg}}
	rec := httptest.NewRecorder()
	s.SetupStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/setup/status", nil))
	if !strings.Contains(rec.Body.String(), `"restart_pending":false`) || !strings.Contains(rec.Body.String(), `"restart_reasons":[]`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// Operations-API design §6.12 R4: the contract is 1.4.0; setup/status names
// restart_pending and restart_reasons without requiring them, and the memory
// commit names its six fields (#779 item 2).
func TestOpsContract794_RestartFieldsAndMemoryCommit(t *testing.T) {
	raw, ok := opsapi.Document("v1")
	if !ok {
		t.Fatal("no v1 document")
	}
	doc := string(raw)
	if !strings.Contains(doc, "  version: 1.4.0") {
		t.Fatal("the contract is not 1.4.0")
	}
	_, rest, _ := strings.Cut(doc, "/api/v1/setup/status:")
	status, _, _ := strings.Cut(rest, "\n  /api/")
	for _, f := range []string{"restart_pending:", "restart_reasons:"} {
		if !strings.Contains(status, f) {
			t.Errorf("setup/status lacks %s", f)
		}
	}
	if !strings.Contains(status, "required: [fresh_install]") {
		t.Error("setup/status's required list changed")
	}
	_, rest, _ = strings.Cut(doc, "/api/v1/setup/session/{id}/memory/commit:")
	commit, _, _ := strings.Cut(rest, "responses:")
	for _, f := range []string{"enabled:", "embedding_endpoint:", "embedding_api_key:", "embedding_model:", "embedding_dimension:", "embedding_provider:"} {
		if !strings.Contains(commit, f) {
			t.Errorf("the memory commit lacks %s", f)
		}
	}
}
