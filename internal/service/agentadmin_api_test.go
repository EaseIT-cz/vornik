package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/apigateway"
)

// Plan P4.5 end to end: add_api is approved on the device, the credential
// is entered on the phone, and a role's query_api reads the API with the
// credential injected by the daemon, through the agent client (not Kong).
// A write from the role route is refused before anything is sent.
func TestAgentAdmin_APIReadEndToEnd(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	var mu sync.Mutex
	var auths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"balance":42}`))
	}))
	t.Cleanup(up.Close)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Money"})
	f.approve(f.do(agentadmin.VerbAddAPI, agentadmin.AddAPIInput{Project: "finance", Name: "fio", BaseURL: up.URL + "/v1",
		Auth: agentadmin.APIAuthInput{Credential: "FIO", Prefix: "Bearer "}, Methods: []string{"GET", "POST"}, Writes: true}))
	if p := f.c.Registry.GetProject("hermes--finance"); p == nil || len(p.APIs) != 1 {
		t.Fatalf("the API did not load: %+v", p)
	}
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "finance", Name: "FIO", Purpose: "balances", Kind: "secret"})
	slot, _ := f.c.repos.ApproverDevices.GetRequest(ctx, requestIDOf(res))
	if err := f.svc.enterCredential(ctx, f.device, *slot, slot.RenderedSHA256, []byte(credCanary)); err != nil {
		t.Fatal(err)
	}
	f.svc.bg.Wait()

	cl := f.c.agentAPIReader("hermes--finance")
	out, err := cl.Call(ctx, apigateway.Request{Provider: "fio", Method: "GET", Path: "accounts/1"})
	if err != nil || !strings.Contains(out.Body, "42") {
		t.Fatalf("read: %+v %v", out, err)
	}
	if _, err := cl.Call(ctx, apigateway.Request{Provider: "fio", Method: "POST", Path: "payments"}); err == nil {
		t.Fatal("the role route made a write")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 1 || auths[0] != "GET /v1/accounts/1 Bearer "+credCanary {
		t.Fatalf("upstream saw %q", auths)
	}
	if lp, ok := cl.(apigateway.ProviderLister); !ok || len(lp.ListProviders()) != 1 {
		t.Fatal("list_apis does not show the approved API")
	}
}

// Review 20261002-2d4f F13: the agent API client the daemon wires dials
// through the SSRF guard: a base URL on a private address is refused.
func TestAgentAPIClient_GuardedDial(t *testing.T) {
	c := &Container{}
	cl := c.agentAPIClient("hermes--fin", false, nil)
	if cl != nil {
		t.Fatal("a client without its dependencies")
	}
	hc := agentDialGuard("https://10.0.0.1/v1").HTTPClient(2 * time.Second)
	if _, err := hc.Get("https://10.0.0.1:1/v1/x"); err == nil || !strings.Contains(err.Error(), "dial guard") {
		t.Fatalf("a private base URL was dialled: %v", err)
	}
	f := newAgentAdminFixture(t)
	wired := f.c.agentAPIClient("hermes--fin", false, map[string]bool{"GET": true})
	if _, err := wired.HTTP("https://10.0.0.1/v1").Get("https://10.0.0.1:1/v1/x"); err == nil || !strings.Contains(err.Error(), "dial guard") {
		t.Fatalf("the wired client dialled a private address: %v", err)
	}
}
