package api

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
)

// GitHub #560 (18-security-and-tenancy.md, "Amendment 2026-10-28 (#560,
// slice M1a)", A4): in managed hosting the public listener refuses every
// admin-class key, in every form a credential is read in, before routing.
// Found at: design (SaaS MVP design 2.2, invariant 1). Integration level: the
// real gate in front of a real handler.

const adminKey = "sk-instance-admin-560"

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// Single-form parity: for each form alone, extractAPIKey returns what that
// form's reader returns, so the gate and the live auth path cannot normalise
// differently.
func TestAPIKeyReaders560_Parity(t *testing.T) {
	cases := []struct {
		name, header, value, want string
	}{
		{"x-api-key", "X-API-Key", "  k1  ", "k1"},
		{"bearer", "Authorization", "Bearer k2", "k2"},
		{"bearer lower case, padded", "Authorization", "  bearer   k3 ", "k3"},
		{"basic", "Authorization", basic("api", "k4"), "k4"},
		{"basic padded", "Authorization", basic("api", "  k5 "), "k5"},
		{"basic with Bearer prefix", "Authorization", basic("api", "Bearer k6"), "k6"},
		{"basic lower-case scheme", "Authorization", "basic " + base64.StdEncoding.EncodeToString([]byte("u:k7")), "k7"},
		{"other scheme", "Authorization", "Digest k8", ""},
		{"empty", "X-API-Key", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set(tc.header, tc.value)
			got := extractAPIKey(r)
			var reader string
			if tc.header == "X-API-Key" {
				reader = apiKeyFromXAPIKey(tc.value)
			} else {
				reader, _ = apiKeyFromAuthorization(tc.value)
			}
			if got != tc.want || reader != tc.want {
				t.Fatalf("extractAPIKey = %q, reader = %q, want %q", got, reader, tc.want)
			}
		})
	}
}

func gated(t *testing.T) (http.Handler, *bool, *bytes.Buffer) {
	t.Helper()
	called := false
	var logs bytes.Buffer
	admin := config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}}
	h := ListenerGate(admin.IsAdminKey, zerolog.New(&logs))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	return h, &called, &logs
}

func TestListenerGate560_AdminKeyRefusedInEveryForm(t *testing.T) {
	forms := []struct {
		name string
		set  func(http.Header)
	}{
		{"X-API-Key", func(h http.Header) { h.Set("X-API-Key", adminKey) }},
		{"X-API-Key padded", func(h http.Header) { h.Set("X-API-Key", "  "+adminKey+" ") }},
		{"Basic password", func(h http.Header) { h.Set("Authorization", basic("api", adminKey)) }},
		{"Basic password as Bearer", func(h http.Header) { h.Set("Authorization", basic("x", "Bearer "+adminKey)) }},
		{"Bearer", func(h http.Header) { h.Set("Authorization", "Bearer "+adminKey) }},
		{"Bearer odd case, padded", func(h http.Header) { h.Set("Authorization", "  bEaReR   "+adminKey+"  ") }},
		{"X-API-Key beside a harmless Authorization", func(h http.Header) {
			h.Set("X-API-Key", adminKey)
			h.Set("Authorization", "Bearer harmless")
		}},
		{"Authorization beside a harmless X-API-Key", func(h http.Header) {
			h.Set("X-API-Key", "harmless")
			h.Set("Authorization", "Bearer "+adminKey)
		}},
		{"second value of a repeated X-API-Key", func(h http.Header) {
			h.Add("X-API-Key", "harmless")
			h.Add("X-API-Key", adminKey)
		}},
		{"second value of a repeated Authorization", func(h http.Header) {
			h.Add("Authorization", "Bearer harmless")
			h.Add("Authorization", basic("api", adminKey))
		}},
	}
	for _, f := range forms {
		t.Run(f.name, func(t *testing.T) {
			h, called, logs := gated(t)
			r := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
			f.set(r.Header)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized || *called {
				t.Fatalf("status %d, handler called %v: an admin-class key passed the public listener", w.Code, *called)
			}
			if strings.Contains(w.Body.String(), adminKey) || strings.Contains(logs.String(), adminKey) {
				t.Fatal("the refusal echoes the key")
			}
			if !strings.Contains(logs.String(), config.KeyHashID(adminKey)) {
				t.Fatalf("the refusal log does not carry the key's hash id: %s", logs.String())
			}
		})
	}

	for name, set := range map[string]func(http.Header){
		"no credential":  func(http.Header) {},
		"a project key":  func(h http.Header) { h.Set("Authorization", "Bearer sk-vornik-project-key") },
		"a plain X key":  func(h http.Header) { h.Set("X-API-Key", "sk-plain") },
		"a session only": func(h http.Header) { h.Set("Cookie", "vornik_session=abc") },
	} {
		t.Run("passes: "+name, func(t *testing.T) {
			h, called, _ := gated(t)
			r := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
			set(r.Header)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if !*called || w.Code != http.StatusNoContent {
				t.Fatalf("status %d, handler called %v: a non-admin request was refused", w.Code, *called)
			}
		})
	}
}

func TestListenerGate560_MetricsOffThePublicListener(t *testing.T) {
	for _, p := range []string{"/metrics", "//metrics", "/metrics/", "/x/../metrics"} {
		h, called, _ := gated(t)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.URL.Path = p
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound || *called {
			t.Errorf("%s on the public listener: status %d, handler called %v", p, w.Code, *called)
		}
	}
}
