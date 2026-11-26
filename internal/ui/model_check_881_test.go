package ui

// GitHub #881, project and swarm registry design §12.2 C2 and C3: a console
// save checks each model it changes. An unreachable model refuses; "Save
// without checking" saves with a warning and is audited; on a managed
// instance a model outside the catalogue is refused even with the override;
// an unchanged model is not checked.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/modelcheck"
)

type uiStubRouter struct {
	down  map[string]bool
	calls map[string]int
}

func (s *uiStubRouter) WithModel(m string) modelcheck.Completer {
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[m]++
	return uiStubModel{down: s.down[m], model: m}
}

type uiStubModel struct {
	down  bool
	model string
}

func (u uiStubModel) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	if u.down {
		return nil, errors.New("gateway error 410: retired")
	}
	r := &chat.ChatResponse{Model: u.model}
	r.Choices = append(r.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "1", Type: "function", Function: chat.FunctionCall{Name: "get_time"}}}}})
	return r, nil
}

func checkFormRequest(vals url.Values) *http.Request {
	r := httptest.NewRequest("POST", "/", strings.NewReader(vals.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = r.ParseForm()
	return r
}

func TestCheckSavedModels_881(t *testing.T) {
	stub := &uiStubRouter{down: map[string]bool{"dead": true}}
	s := &Server{modelChecker: &modelcheck.Checker{Provider: stub}}

	// Unchanged models are not checked; a new good one passes.
	warn, refusal := s.checkSavedModels(checkFormRequest(url.Values{}), []string{"old", "dead"}, []string{"old", "dead", "good", ""})
	if refusal != "" || len(warn) != 0 || stub.calls["old"] != 0 || stub.calls["dead"] != 0 || stub.calls["good"] != 1 {
		t.Fatalf("refusal %q warn %v calls %v", refusal, warn, stub.calls)
	}
	// A new dead one refuses, naming it.
	if _, refusal = s.checkSavedModels(checkFormRequest(url.Values{}), nil, []string{"dead"}); !strings.Contains(refusal, "dead") {
		t.Fatalf("refusal %q", refusal)
	}
	// The override saves with a warning and makes no call.
	calls := stub.calls["dead"]
	warn, refusal = s.checkSavedModels(checkFormRequest(url.Values{"skipModelCheck": {"on"}}), nil, []string{"dead"})
	if refusal != "" || len(warn) == 0 || !strings.Contains(warn[0], "not checked") || stub.calls["dead"] != calls {
		t.Fatalf("override: refusal %q warn %v", refusal, warn)
	}
}

func TestCheckSavedModels_881_ManagedCatalogue(t *testing.T) {
	s := &Server{modelChecker: &modelcheck.Checker{Provider: &uiStubRouter{}},
		managedCatalogue: func() []string { return []string{"open-a"} }}
	for _, skip := range []string{"", "on"} {
		_, refusal := s.checkSavedModels(checkFormRequest(url.Values{"skipModelCheck": {skip}}), nil, []string{"zai.glm-5"})
		if !strings.Contains(refusal, "catalogue") {
			t.Errorf("skip=%q: refusal %q; an off-catalogue model is refused even with the override", skip, refusal)
		}
	}
	if _, refusal := s.checkSavedModels(checkFormRequest(url.Values{}), nil, []string{"open-a"}); refusal != "" {
		t.Errorf("a catalogue model was refused: %s", refusal)
	}
}

func TestSchemaFormModels_881(t *testing.T) {
	r := checkFormRequest(url.Values{
		"roles[0].model": {"a"}, "roles[0].modelFallback": {"b"}, "roles[1].model": {""},
		"roles[1].name": {"x"}, "model": {"top-level-ignored"},
	})
	got := schemaFormModels(r)
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("schemaFormModels = %v", got)
	}
}

// The raw editor's save refuses an unreachable model with 409 and writes
// nothing (the check runs before the lock and the write).
func TestSwarmSave_881_RefusesUnreachableModel(t *testing.T) {
	root := writeSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)
	server.modelChecker = &modelcheck.Checker{Provider: &uiStubRouter{down: map[string]bool{"dead-model": true}}}
	path := filepath.Join(root, "swarms", "edit-swarm.md")
	before, _ := os.ReadFile(path)

	form := url.Values{}
	form.Set("leadRole", "lead")
	form.Set("roleModel_lead", "dead-model") // was: test-lead-model
	form.Set("roleModel_coder", "test-coder-model")
	rec := httptest.NewRecorder()
	server.SwarmSave(rec, postSwarmForm("edit-swarm", form), "edit-swarm")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "dead-model") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) || reloader.calls != 0 {
		t.Fatal("a refused save wrote the swarm or reloaded")
	}
}
