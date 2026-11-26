package api

// GitHub #880, project and swarm registry design §11.2 R6: the profile API.
// GET lists the shipped profiles and marks the one matching the chat
// endpoint; POST .../apply dry-runs by default and, with apply:true, writes
// only runtime.agent_llm.tiers and .roles, from the shipped file, never from
// the request body; GET /api/v1/models/tiers reports each role's resolution.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/modelcheck"
)

const profileYAML = `description: "test"
endpoint_hosts: ["ollama.com"]
tiers:
  complex: {model: "glm-5.2", fallback: "kimi-k2.6"}
roles:
  dev-swarm/lead: {model: "kimi-k2.6"}
`

const profileConfigYAML = `# kept
api:
  auth_enabled: false
chat:
  enabled: true
  endpoint: "https://ollama.com/v1"
  model: "glm-5.2"
runtime:
  agent_llm:
    model: "setup-model"
`

const profileSwarm = "---\nswarmId: dev-swarm\ncoordinatorRole: lead\nroles:\n  - name: lead\n    count: 1\n    runtimePolicy: ephemeral\n    modelTier: complex\n    runtime:\n      image: img\n  - name: gate\n    count: 1\n    runtimePolicy: ephemeral\n    modelTier: trivial\n    runtime:\n      image: img\n---\n"

func profileHandlers(t *testing.T) (*DoctorHandlers, string) {
	t.Helper()
	root := t.TempDir()
	configs := filepath.Join(root, "configs")
	for _, d := range []string{"model-profiles", "swarms"} {
		if err := os.MkdirAll(filepath.Join(configs, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(configs, "model-profiles", "ollama-cloud.yaml"), profileYAML)
	write(filepath.Join(configs, "swarms", "dev-swarm.md"), profileSwarm)
	cfgPath := filepath.Join(root, "config.yaml")
	write(cfgPath, profileConfigYAML)
	h := &DoctorHandlers{configDir: configs, configPath: cfgPath, chatEndpoint: "https://ollama.com/v1"}
	return h, cfgPath
}

func TestModelProfilesList_880(t *testing.T) {
	h, _ := profileHandlers(t)
	rec := httptest.NewRecorder()
	h.ListModelProfiles(rec, authOff(httptest.NewRequest(http.MethodGet, "/api/v1/models/profiles", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Profiles []struct {
			Name    string `json:"name"`
			Matches bool   `json:"matches_endpoint"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Profiles) != 1 || out.Profiles[0].Name != "ollama-cloud" || !out.Profiles[0].Matches {
		t.Fatalf("profiles = %+v", out.Profiles)
	}
}

func applyProfile(t *testing.T, h *DoctorHandlers, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/profiles/"+name+"/apply", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	h.routeModelProfilePath(rec, authOff(req))
	return rec
}

func TestModelProfileApply_880_DryRunWritesNothing(t *testing.T) {
	h, cfgPath := profileHandlers(t)
	rec := applyProfile(t, h, "ollama-cloud", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if raw, _ := os.ReadFile(cfgPath); string(raw) != profileConfigYAML {
		t.Fatalf("a dry run changed config.yaml:\n%s", raw)
	}
	if !strings.Contains(rec.Body.String(), `"applied":false`) || !strings.Contains(rec.Body.String(), "glm-5.2") {
		t.Fatalf("dry run body: %s", rec.Body)
	}
}

func TestModelProfileApply_880_WritesOnlyTheProfile(t *testing.T) {
	h, cfgPath := profileHandlers(t)
	// A body that carries models is refused outright: values come only from
	// the shipped file.
	rec := applyProfile(t, h, "ollama-cloud", `{"apply":true,"tiers":{"complex":{"model":"attacker-model"}}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a body carrying tiers: status %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if raw, _ := os.ReadFile(cfgPath); string(raw) != profileConfigYAML {
		t.Fatalf("a refused body changed config.yaml:\n%s", raw)
	}
	rec = applyProfile(t, h, "ollama-cloud", `{"apply":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	raw, _ := os.ReadFile(cfgPath)
	text := string(raw)
	if strings.Contains(text, "attacker-model") {
		t.Fatalf("a model from the request body reached config.yaml:\n%s", text)
	}
	for _, want := range []string{"# kept", "setup-model", "glm-5.2", "kimi-k2.6", "dev-swarm/lead"} {
		if !strings.Contains(text, want) {
			t.Errorf("config.yaml lacks %q after apply:\n%s", want, text)
		}
	}
	if !strings.Contains(rec.Body.String(), `"restart_required":true`) {
		t.Errorf("apply body does not ask for a restart: %s", rec.Body)
	}
}

func TestModelProfileApply_880_Refusals(t *testing.T) {
	h, cfgPath := profileHandlers(t)
	for _, tc := range []struct {
		name, method string
		want         int
	}{
		{"nope", http.MethodPost, http.StatusNotFound},
		{"..%2f..%2fconfig", http.MethodPost, http.StatusNotFound},
		{"ollama-cloud", http.MethodGet, http.StatusMethodNotAllowed},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, "/api/v1/models/profiles/"+tc.name+"/apply", bytes.NewBufferString(`{"apply":true}`))
		h.routeModelProfilePath(rec, authOff(req))
		if rec.Code != tc.want {
			t.Errorf("%s %s: status %d, want %d (%s)", tc.method, tc.name, rec.Code, tc.want, rec.Body)
		}
	}
	// Auth on and no admin-class principal: refused, nothing written.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/profiles/ollama-cloud/apply", bytes.NewBufferString(`{"apply":true}`))
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, true))
	h.routeModelProfilePath(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("a non-admin request applied a profile")
	}
	if raw, _ := os.ReadFile(cfgPath); string(raw) != profileConfigYAML {
		t.Fatalf("a refused request changed config.yaml")
	}
}

func TestModelTiers_880(t *testing.T) {
	h, _ := profileHandlers(t)
	rec := httptest.NewRecorder()
	h.ModelTiers(rec, authOff(httptest.NewRequest(http.MethodGet, "/api/v1/models/tiers", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Roles []struct {
			Swarm, Role, Tier, Model, Source string
		} `json:"roles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Roles) != 2 || out.Roles[0].Role != "gate" || out.Roles[0].Tier != "trivial" || out.Roles[1].Role != "lead" {
		t.Fatalf("roles = %+v", out.Roles)
	}
}

// GitHub #881, registry design §12.2 C2: apply checks every model the profile
// names. An unreachable one refuses and writes nothing; force skips only the
// check (the values still come from the file); a text-only answer saves with
// a warning.
type stubModelRouter struct{ answers map[string]error }

func (s stubModelRouter) WithModel(m string) modelcheck.Completer {
	return stubModel{err: s.answers[m], model: m}
}

type stubModel struct {
	err   error
	model string
}

func (s stubModel) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	r := &chat.ChatResponse{Model: s.model}
	r.Choices = append(r.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "1", Type: "function", Function: chat.FunctionCall{Name: "get_time"}}}}})
	return r, nil
}

func TestModelProfileApply_881_ChecksModels(t *testing.T) {
	h, cfgPath := profileHandlers(t) // the profile names glm-5.2 and kimi-k2.6
	h.modelChecker = &modelcheck.Checker{Provider: stubModelRouter{answers: map[string]error{
		"kimi-k2.6": &chat.GatewayError{Status: 410, Message: "retired"},
	}}, RouteOf: func(string) string { return "ollama" }}

	rec := applyProfile(t, h, "ollama-cloud", `{"apply":true}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "kimi-k2.6") || !strings.Contains(rec.Body.String(), "MODEL_UNREACHABLE") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if raw, _ := os.ReadFile(cfgPath); string(raw) != profileConfigYAML {
		t.Fatal("a refused apply changed config.yaml")
	}

	rec = applyProfile(t, h, "ollama-cloud", `{"apply":true,"force":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not checked") {
		t.Fatalf("force: status %d: %s", rec.Code, rec.Body)
	}
	if raw, _ := os.ReadFile(cfgPath); !strings.Contains(string(raw), "kimi-k2.6") {
		t.Fatal("force did not write the profile")
	}
}

// GitHub #881 C3: on a managed instance a profile model outside the catalogue
// is refused even with force.
func TestModelProfileApply_881_ManagedCatalogue(t *testing.T) {
	h, cfgPath := profileHandlers(t) // the profile names glm-5.2 and kimi-k2.6
	h.SetModelChecker(nil, func() []string { return []string{"glm-5.2"} })
	rec := applyProfile(t, h, "ollama-cloud", `{"apply":true,"force":true}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "kimi-k2.6") || !strings.Contains(rec.Body.String(), "catalogue") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if raw, _ := os.ReadFile(cfgPath); string(raw) != profileConfigYAML {
		t.Fatal("a refused apply changed config.yaml")
	}
}
