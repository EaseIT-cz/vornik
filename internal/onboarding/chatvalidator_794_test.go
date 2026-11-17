package onboarding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// GitHub #794 (security design 18, amendment 2026-11-16, P4/F1): the owner's
// probe on a managed instance dials under managed mode's run-time rule, not
// setup's AllowPrivate rule, so the page cannot reach the instance's private
// network. These tests would pass against setup's validator only if it
// refused loopback, which it does not.

func TestManagedChatValidator794_RefusesLoopback(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()

	v := NewManagedChatValidator([]string{"gateway.example"})
	if _, err := v.ListModels(context.Background(), srv.URL, "key"); err == nil {
		t.Fatal("the managed probe listed models from a loopback server")
	}
	res := v.Validate(context.Background(), ChatConfigProposal{Endpoint: srv.URL, APIKey: "key", Model: "m"})
	if res.PingOK || res.EndpointOK {
		t.Fatalf("the managed probe reached a loopback server: %+v", res)
	}
	if hits != 0 {
		t.Fatalf("the loopback server was hit %d times", hits)
	}

	// The contrast that makes this a regression test: setup's validator,
	// which admits private and loopback addresses, does reach it.
	if _, err := NewChatValidator().ListModels(context.Background(), srv.URL, "key"); err != nil {
		t.Fatalf("setup's validator should reach loopback (its rule admits it): %v", err)
	}
}

func TestManagedChatValidator794_GuardCarriesTheVouchedHosts(t *testing.T) {
	hosts := []string{"gateway.example", "models.internal.example"}
	g := managedProbeGuard(hosts)
	if g.AllowPrivate {
		t.Fatal("the managed probe guard admits every private address")
	}
	if !slices.Equal(g.PrivateHosts, hosts) {
		t.Fatalf("PrivateHosts = %v, want %v", g.PrivateHosts, hosts)
	}
	if len(g.AllowedHosts) != 0 || len(g.LoopbackHosts) != 0 {
		t.Fatalf("the guard skips address checks for some host: %+v", g)
	}
}
