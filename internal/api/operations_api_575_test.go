package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/opsapi"
)

// GitHub #575 (2026-10-28-operations-api-contract-design.md, C4.1): every
// operation the operations API documents is routed to its own pattern and,
// inside setupSessionRouter, to its own action; it answers 401 with no key and
// is neither 401 nor 403 with the instance_admin key; and it does not answer
// 405 for its documented method. Integration level: the production router
// (NewRouter: auth middleware, the real mux and handlers).

const opsAdminKey575 = "sk-vornik-instance-admin-575"

type docOperation575 struct {
	method, path string
	public       bool // security: [] (no credential)
}

// documentedOperations575 lists every operation of a served major, with
// whether it is unauthenticated.
func documentedOperations575(t *testing.T, major string) []docOperation575 {
	t.Helper()
	doc, ok := opsapi.Document(major)
	if !ok {
		t.Fatalf("no document for %s", major)
	}
	var d struct {
		Paths map[string]map[string]struct {
			Security *[]map[string][]string `yaml:"security"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(doc, &d); err != nil {
		t.Fatal(err)
	}
	var out []docOperation575
	for path, item := range d.Paths {
		for m, op := range item {
			if !slices.Contains(opsapi.Methods(), m) {
				continue // a path-level key (parameters, summary), not an operation
			}
			public := op.Security != nil && len(*op.Security) == 0
			out = append(out, docOperation575{method: strings.ToUpper(m), path: path, public: public})
		}
	}
	slices.SortFunc(out, func(a, b docOperation575) int { return strings.Compare(a.method+a.path, b.method+b.path) })
	return out
}

func operationsRig575(t *testing.T, ollama bool) *Router {
	t.Helper()
	prev := doctorHandlers
	SetDoctorHandlers(NewDoctorHandlers(nil))
	t.Cleanup(func() { SetDoctorHandlers(prev) })
	cfg := &config.Config{}
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{opsAdminKey575}
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{opsAdminKey575}}
	opts := []ServerOption{WithLogger(zerolog.Nop()), WithConfig(cfg), WithAdminConfig(cfg.Admin)}
	if ollama {
		opts = append(opts, WithChatProvider(&streamingStub{model: "stub"}))
	}
	return NewRouter(NewServer(opts...), cfg)
}

// concrete fills {id} with a value no session has.
func concrete575(path string) string { return strings.ReplaceAll(path, "{id}", "no-such-session-575") }

// ownPattern is the ServeMux pattern an operation must match: its path, or for
// a {id} path the subtree registered for it.
func ownPattern575(path string) string {
	if i := strings.Index(path, "{id}"); i >= 0 {
		return path[:i]
	}
	return path
}

func TestOperationsAPI575_EveryOperationIsRouted(t *testing.T) {
	for _, ollama := range []bool{true, false} {
		r := operationsRig575(t, ollama)
		h := recoverPanic467(r.Handler())
		examined := 0
		for _, major := range opsapi.Served() {
			for _, op := range documentedOperations575(t, major) {
				if op.path == "/metrics" {
					continue // the root mux's; TestManagedListeners560 routes it (C4.1)
				}
				examined++
				name := major + " " + op.method + " " + op.path
				req := httptest.NewRequest(op.method, concrete575(op.path), strings.NewReader("{}"))
				if got, want := r.RoutePattern(req), ownPattern575(op.path); got != want {
					t.Errorf("%s (ollama %v): matched pattern %q, want %q", name, ollama, got, want)
				}
				if leaf, ok := strings.CutPrefix(op.path, "/api/v1/setup/session/{id}/"); ok {
					if got := setupSessionAction(concrete575(op.path)); got != leaf {
						t.Errorf("%s: setupSessionAction = %q, want %q", name, got, leaf)
					}
				}
				rec := httptest.NewRecorder()
				req = httptest.NewRequest(op.method, concrete575(op.path), strings.NewReader("{}"))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+opsAdminKey575)
				h.ServeHTTP(rec, req)
				if rec.Code == http.StatusMethodNotAllowed || rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
					t.Errorf("%s (ollama %v) with instance_admin: %d %s", name, ollama, rec.Code, rec.Body.String())
				}
				if op.public {
					continue
				}
				rec = httptest.NewRecorder()
				req = httptest.NewRequest(op.method, concrete575(op.path), strings.NewReader("{}"))
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("%s (ollama %v) with no key: %d, want 401", name, ollama, rec.Code)
				}
			}
		}
		if examined == 0 {
			t.Fatal("no documented operation examined")
		}
		t.Logf("ollama %v: %d documented operations examined", ollama, examined)
	}
}

// The probe can fail: an undocumented path matches the catch-all (Ollama on)
// or nothing (off); an unknown suffix, and a documented one whose case is
// removed, do not map to their own action; a wrong method answers 405.
func TestOperationsAPI575_ProbeCanFail(t *testing.T) {
	on, off := operationsRig575(t, true), operationsRig575(t, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/not-documented-575", nil)
	if got := on.RoutePattern(req); got != "/" {
		t.Errorf("ollama on: undocumented path matched %q, want /", got)
	}
	if got := off.RoutePattern(req); got != "" {
		t.Errorf("ollama off: undocumented path matched %q, want none", got)
	}
	if got := setupSessionAction("/api/v1/setup/session/x/rollback"); got != "" {
		t.Errorf("unknown suffix: %q", got)
	}
	without := slices.DeleteFunc(slices.Clone(setupSessionActions), func(a string) bool { return a == "memory/commit" })
	if got := setupSessionActionIn(without, "/api/v1/setup/session/x/memory/commit"); got == "memory/commit" {
		t.Error("with the memory/commit entry removed, memory/commit still mapped to itself")
	}
	rec := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/daemon/drain", nil)
	req.Header.Set("Authorization", "Bearer "+opsAdminKey575)
	on.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET drain: %d, want 405", rec.Code)
	}
}

func TestOperationsAPI575_CapabilitiesListsServedMajors(t *testing.T) {
	r := operationsRig575(t, false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+opsAdminKey575)
	r.Handler().ServeHTTP(rec, req)
	var got struct {
		OperationsAPI []string `json:"operationsAPI"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !slices.Equal(got.OperationsAPI, opsapi.Served()) {
		t.Fatalf("capabilities operationsAPI %v (%v), want %v", got.OperationsAPI, err, opsapi.Served())
	}
}
