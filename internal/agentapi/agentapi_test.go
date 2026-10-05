package agentapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/registry"
)

// It carries characters escaping changes, and its length is not a multiple
// of three, so every encoded form differs from the raw one.
const credCanary = "CANARY/api+key 9c2?x"

type grantRows struct {
	persistence.AgentGrantRepository
	rows map[string]*persistence.AgentIntegrationApproval
}

func (g grantRows) GetIntegration(_ context.Context, projectID, name string) (*persistence.AgentIntegrationApproval, error) {
	if r, ok := g.rows[projectID+"/"+name]; ok {
		return r, nil
	}
	return nil, persistence.ErrNotFound
}

type secrets map[string]string

func (s secrets) Get(name string) (string, bool) { v, ok := s[name]; return v, ok }

type upstream struct {
	mu   sync.Mutex
	seen []*http.Request
	body []string
}

func newUpstream(t *testing.T) (*httptest.Server, *upstream) {
	u := &upstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.seen = append(u.seen, r)
		u.body = append(u.body, string(b))
		u.mu.Unlock()
		switch r.URL.Path {
		case "/v1/redirect":
			http.Redirect(w, r, "http://10.0.0.1:1/x", http.StatusFound)
		case "/v1/echo-key":
			// Every encoded form of the header and of the bare key, one
			// field each (review 20261002-2acb F4: the whole form matrix).
			forms := encodedForms(r.Header.Get("Authorization"))
			forms = append(forms, encodedForms(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))...)
			out, _ := json.Marshal(map[string][]string{"echo": forms})
			_, _ = w.Write(out)
		case "/v1/big":
			_, _ = w.Write(bytes.Repeat([]byte("x"), 2<<20))
		default:
			_, _ = w.Write([]byte(`{"balance":42}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, u
}

func newClient(t *testing.T, base string, write bool, allowed ...string) (*Client, *bytes.Buffer, grantRows) {
	t.Helper()
	p := &registry.Project{ID: "hermes--fin", APIs: []registry.ProjectAPI{{Name: "fio", BaseURL: base + "/v1",
		Methods: []string{"GET", "POST"}, Writes: true,
		Auth: registry.ProjectAPIAuth{Header: "Authorization", ValueFrom: "secret://hermes/FIO", Prefix: "Bearer "}}}}
	g := grantRows{rows: map[string]*persistence.AgentIntegrationApproval{
		"hermes--fin/fio": {ProjectID: "hermes--fin", Integration: "fio", Kind: "api", ReadTools: []string{"GET"}, WriteTools: []string{"POST"}},
	}}
	var logs bytes.Buffer
	am := map[string]bool{}
	for _, m := range allowed {
		am[m] = true
	}
	c := &Client{ProjectID: "hermes--fin", Project: func(string) *registry.Project { return p }, Grants: g,
		Secrets: secrets{"hermes/FIO": credCanary}, AllowedMethods: am, Write: write,
		HTTP: func(string) *http.Client { return &http.Client{Timeout: 5 * time.Second} }, Logger: zerolog.New(&logs)}
	return c, &logs, g
}

// Design §8.3, plan P4.5: an approved read passes with the injected header;
// everything else is refused before anything is sent. Control: Client.Call.
func TestClient_ReadPathAndRefusals(t *testing.T) {
	srv, up := newUpstream(t)
	c, logs, g := newClient(t, srv.URL, false, "GET", "HEAD")
	res, err := c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: "GET", Path: "accounts/1/balance", Query: map[string]any{"from": "2026-09"}})
	if err != nil || res.Status != 200 || !strings.Contains(res.Body, "42") {
		t.Fatalf("an approved GET: %+v %v", res, err)
	}
	if got := up.seen[0]; got.Header.Get("Authorization") != "Bearer "+credCanary || got.URL.Path != "/v1/accounts/1/balance" || got.URL.Query().Get("from") != "2026-09" {
		t.Fatalf("upstream saw %s %s %q", got.Method, got.URL, got.Header.Get("Authorization"))
	}
	sent := len(up.seen)
	for name, req := range map[string]apigateway.Request{
		"POST on the read route":   {Provider: "fio", Method: "POST", Path: "x"},
		"unknown provider":         {Provider: "mail", Method: "GET", Path: "x"},
		"dot-dot":                  {Provider: "fio", Method: "GET", Path: "a/../../admin"},
		"encoded dot-dot":          {Provider: "fio", Method: "GET", Path: "a/%2e%2e/admin"},
		"absolute URL in the path": {Provider: "fio", Method: "GET", Path: "https://evil.example/x"},
		"protocol-relative path":   {Provider: "fio", Method: "GET", Path: "//evil.example/x"},
		// Review 20261002-2d4f F1: double encodings an upstream may decode twice.
		"double-encoded dot-dot":   {Provider: "fio", Method: "GET", Path: "..%252fadmin"},
		"double-encoded backslash": {Provider: "fio", Method: "GET", Path: "a%255c..%255cadmin"},
		"encoded slash to dot-dot": {Provider: "fio", Method: "GET", Path: "a%2f..%2f..%2fadmin"}, // regression guard
		// Review 20261002-c65a F1: a path parameter an upstream strips.
		"dot-dot with a path parameter": {Provider: "fio", Method: "GET", Path: "..;z/admin"},
		"path parameter in a segment":   {Provider: "fio", Method: "GET", Path: "a/b;mat=../c"},
		"encoded path parameter":        {Provider: "fio", Method: "GET", Path: "..%3Bz/admin"},
		"empty segment":                 {Provider: "fio", Method: "GET", Path: "a//b"},
	} {
		if _, err := c.Call(context.Background(), req); err == nil {
			t.Errorf("%s: sent", name)
		}
	}
	if len(up.seen) != sent {
		t.Fatalf("a refused call reached the upstream (%d requests)", len(up.seen)-sent)
	}
	// Ordinary single-encoded paths still pass, each segment sent escaped
	// exactly once (review 20261002-2acb suggestion 1).
	for raw, want := range map[string]string{"files/foo%20bar": "/v1/files/foo%20bar", "files/a%2Fb": "/v1/files/a/b", "files/x-y_z.json": "/v1/files/x-y_z.json"} {
		if _, err := c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: "GET", Path: raw}); err != nil {
			t.Errorf("%s: refused: %v", raw, err)
			continue
		}
		if got := up.seen[len(up.seen)-1].URL.EscapedPath(); got != want {
			t.Errorf("%s: sent %s, want %s", raw, got, want)
		}
	}
	// A redirect to another host is not followed.
	if res, err := c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: "GET", Path: "redirect"}); err == nil && res.Status != http.StatusFound {
		t.Fatalf("a cross-host redirect was followed: %+v", res)
	}
	// The credential echoed back is scrubbed, raw and encoded (review
	// 20261002-2d4f F5); a big body is capped.
	res2, _ := c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: "GET", Path: "echo-key"})
	for _, enc := range append(encodedForms("Bearer "+credCanary), encodedForms(credCanary)...) {
		if strings.Contains(res2.Body, enc) {
			t.Fatalf("the credential came back in the body as %q: %s", enc, res2.Body)
		}
	}
	// A path with a single, ordinary encoding still works.
	if _, err := c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: "GET", Path: "names/J%C3%B6rg"}); err != nil {
		t.Fatalf("a single-encoded path: %v", err)
	}
	if res, _ := c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: "GET", Path: "big"}); len(res.Body) > MaxBodyBytes+64 {
		t.Fatalf("the body was not capped: %d", len(res.Body))
	}
	// A removed approval refuses.
	g.rows["hermes--fin/fio"].RemovedAt = &time.Time{}
	if _, err := c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: "GET", Path: "x"}); !errors.Is(err, apigateway.ErrUnknownProvider) {
		t.Fatalf("a removed approval: %v", err)
	}
	if strings.Contains(logs.String(), credCanary) || strings.Contains(logs.String(), "balance") || strings.Contains(logs.String(), "2026-09") {
		t.Fatalf("the log carries a value, a path or a body: %s", logs.String())
	}
}

// Some legacy APIs (notably Google Maps Platform legacy web services) require
// API-key authentication as a query parameter instead of a header. The agent
// must still pass no credential in its query_api arguments; the daemon injects
// the key after the egress scan and before the upstream request.
func TestClient_QueryParamAuth(t *testing.T) {
	srv, up := newUpstream(t)
	p := &registry.Project{ID: "hermes--places", APIs: []registry.ProjectAPI{{Name: "maps", BaseURL: srv.URL,
		Methods: []string{"GET"},
		Auth:    registry.ProjectAPIAuth{QueryParam: "key", ValueFrom: "secret://hermes/GOOGLE_MAPS"}}}}
	g := grantRows{rows: map[string]*persistence.AgentIntegrationApproval{
		"hermes--places/maps": {ProjectID: "hermes--places", Integration: "maps", Kind: "api", ReadTools: []string{"GET"}},
	}}
	var logs bytes.Buffer
	c := &Client{ProjectID: "hermes--places", Project: func(string) *registry.Project { return p }, Grants: g,
		Secrets: secrets{"hermes/GOOGLE_MAPS": credCanary}, AllowedMethods: map[string]bool{"GET": true},
		HTTP: func(string) *http.Client { return &http.Client{Timeout: 5 * time.Second} }, Logger: zerolog.New(&logs)}

	_, err := c.Call(context.Background(), apigateway.Request{
		Provider: "maps", Method: "GET", Path: "maps/api/place/textsearch/json",
		Query: map[string]any{"query": "coffee in Prague", "key": "caller-supplied"},
	})
	if err != nil {
		t.Fatalf("query-param auth call: %v", err)
	}
	got := up.seen[0]
	if got.Header.Get("Authorization") != "" {
		t.Fatalf("query-param auth also sent an Authorization header: %q", got.Header.Get("Authorization"))
	}
	if got.URL.Query().Get("query") != "coffee in Prague" || got.URL.Query().Get("key") != credCanary {
		t.Fatalf("upstream query = %v", got.URL.Query())
	}
	if strings.Contains(logs.String(), credCanary) || strings.Contains(logs.String(), "caller-supplied") {
		t.Fatalf("the query-param credential or request query leaked into logs: %s", logs.String())
	}
}

// The worker's write route: exactly the approved write method, against the
// live WRITE set (review 6e86 F4).
func TestClient_WriteRoute(t *testing.T) {
	srv, up := newUpstream(t)
	c, _, g := newClient(t, srv.URL, true, "POST")
	if _, err := c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: "POST", Path: "payments", Body: map[string]any{"amount": 5}}); err != nil {
		t.Fatalf("an approved write: %v", err)
	}
	if up.seen[0].Method != "POST" || !strings.Contains(up.body[0], `"amount":5`) {
		t.Fatalf("upstream saw %s %q", up.seen[0].Method, up.body[0])
	}
	g.rows["hermes--fin/fio"].WriteTools = nil
	if _, err := c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: "POST", Path: "payments"}); !errors.Is(err, apigateway.ErrMethodNotAllowed) {
		t.Fatalf("a method dropped from the write set: %v", err)
	}
	if _, err := c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: "GET", Path: "x"}); err == nil {
		t.Fatal("the write route made a read")
	}
}

// list_apis shows only APIs whose approval is live, with this route's methods.
func TestClient_ListProviders(t *testing.T) {
	c, _, g := newClient(t, "https://a.example", false, "GET", "HEAD")
	got := c.ListProviders()
	if len(got) != 1 || got[0].Name != "fio" || strings.Join(got[0].AllowedMethods, ",") != "GET" {
		t.Fatalf("providers %+v", got)
	}
	g.rows["hermes--fin/fio"].RemovedAt = &time.Time{}
	if got := c.ListProviders(); len(got) != 0 {
		t.Fatalf("a removed API is listed: %+v", got)
	}
}

// The test double keeps the repository's miss contract.
func TestGrantRows_MissContract(t *testing.T) {
	repotest.AssertMiss(t, "AgentGrantRepository.GetIntegration", func() (*persistence.AgentIntegrationApproval, error) {
		return grantRows{}.GetIntegration(context.Background(), "p", "absent")
	})
}

// encodedForms is every form an upstream might echo a credential in: raw,
// query- and path-escaped, and base64 in both alphabets, padded or not.
func encodedForms(v string) []string {
	b := []byte(v)
	return []string{v, url.QueryEscape(v), url.PathEscape(v),
		base64.StdEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b)}
}
