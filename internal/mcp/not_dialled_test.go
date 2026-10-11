package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// GitHub #91 (2026-10-08): an oauth server with no grant was dialled and
// 401'd on every probe. A config carrying NotDialledReason is never
// connected, and the registry reports it in its own state, not as an
// overloaded reachable=false (auth design §14, review 5fe0 F4/F6).
func TestNotDialled_NeverConnectsAndHasItsOwnState(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "invalid_token", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	reason := "not connected, no grant: run vornikctl mcp connect atlassian"
	cfg := ServerConfig{Name: "atlassian", Transport: "streamable-http", URL: srv.URL, NotDialledReason: reason}

	if _, err := Connect(context.Background(), cfg, zerolog.Nop()); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("Connect of a not-dialled server = %v", err)
	}
	r := NewRegistry([]ServerConfig{cfg}, time.Hour, zerolog.Nop())
	r.RefreshAll(context.Background())
	_ = r.Snapshot(context.Background())
	time.Sleep(50 * time.Millisecond) // any async refresh Snapshot spawned
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests reached a server with no grant", n)
	}
	snap := r.Snapshot(context.Background())
	if len(snap) != 1 || !snap[0].NotDialled || snap[0].Reachable || !strings.HasPrefix(snap[0].Error, "not dialled: ") || !strings.Contains(snap[0].Error, reason) {
		t.Fatalf("snapshot = %+v", snap)
	}
	if got := r.NotDialled(); len(got) != 1 || got[0] != "atlassian" {
		t.Fatalf("NotDialled() = %v", got)
	}

	// Once the reason clears (a grant was stored), the server is dialled.
	cfg.NotDialledReason = ""
	r.SetServers([]ServerConfig{cfg})
	if s := r.Snapshot(context.Background()); s[0].NotDialled {
		t.Fatalf("the not-dialled state outlived its reason: %+v", s[0])
	}
	r.RefreshAll(context.Background())
	if hits.Load() == 0 {
		t.Fatal("a server whose reason cleared was not dialled")
	}
}

// F6: the reason is wiring state, never wire shape; the snapshot carries
// not_dialled only when true.
func TestNotDialled_JSONShape(t *testing.T) {
	raw, _ := json.Marshal(ServerConfig{Name: "x", Transport: "sse", URL: "https://x.example", NotDialledReason: "r"})
	if strings.Contains(strings.ToLower(string(raw)), "dialled") {
		t.Fatalf("ServerConfig JSON carries the reason: %s", raw)
	}
	plain, _ := json.Marshal(ServerSnapshot{Name: "x"})
	if strings.Contains(string(plain), "not_dialled") {
		t.Fatalf("a dialled snapshot carries not_dialled: %s", plain)
	}
	nd, _ := json.Marshal(ServerSnapshot{Name: "x", NotDialled: true})
	if !strings.Contains(string(nd), `"not_dialled":true`) {
		t.Fatalf("a not-dialled snapshot lacks not_dialled: %s", nd)
	}
}
