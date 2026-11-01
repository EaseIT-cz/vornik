package mcpauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// GitHub #566 (mcp-server-authentication-design.md §16.2 R4): an exchange
// error never carries the request's code, verifier, refresh token or client
// secret, even when the token endpoint echoes them, in its error field or in
// an error_description where a value straddles the 200-character cut. Found
// at: design review (review-20261028-4db4 F3); regression test at: unit.

// echoingTokenEndpoint answers every token request with a 400 whose error
// object repeats the request's secret values.
func echoingTokenEndpoint(t *testing.T, place func(form map[string]string) (errField, desc string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		e, d := place(form)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": e, "error_description": d})
	}))
}

func TestExchangeCode566_RedactsItsRequestFromErrors(t *testing.T) {
	const code, verifier, secret, refresh = "4/0AbCdEf+the/code=", "verifier-abcdefghijklmnopqrstuvwxyz0123456789ABCDEF", "the+client/secret=", "the/refresh+token="
	creds := ClientCredentials{ID: "client", Secret: secret}
	pkce := PKCE{Verifier: verifier, Challenge: "c"}
	cases := map[string]func(map[string]string) (string, string){
		"in error_description": func(f map[string]string) (string, string) {
			return "invalid_request", "got " + f["code"] + " " + f["code_verifier"] + " " + f["client_secret"] + " " + f["refresh_token"]
		},
		"across the 200-character cut": func(f map[string]string) (string, string) {
			v := f["code_verifier"] + f["refresh_token"]
			return "invalid_request", strings.Repeat("x", 200-len(v)/2) + v
		},
		"in the error field": func(f map[string]string) (string, string) {
			return f["code_verifier"] + f["refresh_token"] + f["client_secret"], "nothing here"
		},
	}
	// Review 8c0e F1: the endpoint echoes the raw form body it received, in
	// which every value is percent-encoded.
	cases["the raw form body"] = func(f map[string]string) (string, string) {
		form := url.Values{}
		for k, v := range f {
			form.Set(k, v)
		}
		return "invalid_request", form.Encode()
	}
	for name, place := range cases {
		t.Run(name, func(t *testing.T) {
			srv := echoingTokenEndpoint(t, place)
			defer srv.Close()
			md := Metadata{TokenEndpoint: srv.URL + "/token", Resource: "https://r"}
			_, err := ExchangeCode(context.Background(), srv.Client(), md, creds, "https://cb", code, pkce)
			assertNoSecret(t, "ExchangeCode", err, code, verifier, secret)
			if name == "in error_description" && !strings.Contains(err.Error(), "invalid_request") {
				t.Fatalf("redaction swallowed the non-secret error code too: %v", err)
			}
			_, err = Refresh(context.Background(), srv.Client(), md, creds, refresh)
			assertNoSecret(t, "Refresh", err, refresh, secret)
		})
	}
}

func assertNoSecret(t *testing.T, call string, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: the echoing endpoint's 400 was not an error", call)
	}
	for _, secret := range secrets {
		// A fragment as short as 12 characters of a secret, in any spelling
		// an endpoint could echo it in, is a leak too.
		for _, s := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
			for i := 0; i+12 <= len(s); i++ {
				if strings.Contains(err.Error(), s[i:i+12]) {
					t.Fatalf("%s: the error carries part of a request value: %v", call, err)
				}
			}
		}
	}
}

// R3: the routed state names the instance and keeps NewState's entropy; an id
// the pattern refuses is refused.
func TestNewRoutedState566(t *testing.T) {
	s, err := NewRoutedState("acme-2")
	if err != nil {
		t.Fatal(err)
	}
	id, rnd, ok := strings.Cut(s, ".")
	if !ok || id != "acme-2" || len(rnd) != 32 || strings.ContainsAny(rnd, ".+/=") {
		t.Fatalf("routed state %q, want acme-2.<32 base64url characters>", s)
	}
	// The random part is NewState's: never repeated (review bcf3 F1).
	seen := map[string]bool{}
	for range 64 {
		s, err := NewRoutedState("acme-2")
		if err != nil || seen[s] {
			t.Fatalf("a routed state repeated or failed: %q %v", s, err)
		}
		seen[s] = true
	}
	for _, bad := range []string{"", "ac.me", "Acme", "-acme", "acme_1"} {
		if _, err := NewRoutedState(bad); err == nil {
			t.Errorf("NewRoutedState(%q) accepted an id the pattern refuses", bad)
		}
	}
}

// Review 8c0e F2: a client secret or refresh token has no lower length bound
// in any spec, so a short one is redacted too.
func TestExchangeCode566_RedactsShortSecrets(t *testing.T) {
	srv := echoingTokenEndpoint(t, func(f map[string]string) (string, string) {
		return "invalid_client", "secret=" + f["client_secret"] + " refresh=" + f["refresh_token"]
	})
	defer srv.Close()
	md := Metadata{TokenEndpoint: srv.URL + "/token", Resource: "https://r"}
	_, err := Refresh(context.Background(), srv.Client(), md, ClientCredentials{ID: "client", Secret: "s3cr"}, "rt1")
	if err == nil || strings.Contains(err.Error(), "s3cr") || strings.Contains(err.Error(), "refresh=rt1") {
		t.Fatalf("a short secret or refresh token reached the error: %v", err)
	}
}

// Review 07e9 F1: an endpoint whose JSON encoder escapes the solidus (legal,
// and some stacks do) writes "\\/" for every "/"; Go's own encoder never does,
// so the body is written by hand.
func TestExchangeCode566_RedactsSolidusEscapedEchoes(t *testing.T) {
	const secret = "the/client/secret/value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		escaped := strings.ReplaceAll(r.PostForm.Get("client_secret"), "/", `\/`)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"got ` + escaped + `"}`))
	}))
	defer srv.Close()
	md := Metadata{TokenEndpoint: srv.URL + "/token", Resource: "https://r"}
	_, err := Refresh(context.Background(), srv.Client(), md, ClientCredentials{ID: "client", Secret: secret}, "refresh-token-value")
	if err == nil || strings.Contains(err.Error(), "client/secret") || strings.Contains(err.Error(), "the/client") {
		t.Fatalf("a solidus-escaped secret reached the error: %v", err)
	}
}
