package service

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #893 (agent-administered design part 06 §6.11): the source of the
// doctor finding agent_connections_cgnat. It examines live approved agent
// connections only, counts an address in 100.64.0.0/10 that a
// private_api_hosts grant can admit (so 100.100.100.200, refused even when
// listed, is not counted), says whether each host is on
// agent_admin.private_api_hosts, and names the hosts it could not resolve.
// Since #893 option A the one list refuses the range, so the count no longer
// follows IsBlockedIP: the same rows are found before and after the change.
func TestAgentCGNATConnections_893(t *testing.T) {
	removed := time.Now()
	rows := map[string][]persistence.AgentIntegrationApproval{
		"hermes": {
			{Namespace: "hermes", ProjectID: "hermes-home", Integration: "literal", Kind: "api", URL: "https://100.100.1.2/api"},
			{Namespace: "hermes", ProjectID: "hermes-home", Integration: "nas", Kind: "mcp", URL: "https://nas.tail1234.ts.net/mcp"},
			{Namespace: "hermes", ProjectID: "hermes-home", Integration: "ha", Kind: "mcp", URL: "https://HA.lan.example./mcp"},
			{Namespace: "hermes", ProjectID: "hermes-home", Integration: "gone", Kind: "api", URL: "https://100.64.0.5/", RemovedAt: &removed},
			{Namespace: "hermes", ProjectID: "hermes-home", Integration: "meta", Kind: "api", URL: "https://100.100.100.200/"},
			{Namespace: "hermes", ProjectID: "hermes-home", Integration: "meta6", Kind: "api", URL: "https://meta.mapped.example/"},
			{Namespace: "hermes", ProjectID: "hermes-home", Integration: "public", Kind: "api", URL: "https://api.example.com/v1"},
		},
		"other": {
			{Namespace: "other", ProjectID: "other-x", Integration: "dark", Kind: "mcp", URL: "https://nowhere.invalid/mcp"},
		},
	}
	list := func(_ context.Context, ns string) ([]persistence.AgentIntegrationApproval, error) {
		return rows[ns], nil
	}
	dns := map[string][]string{
		"nas.tail1234.ts.net": {"100.101.1.2", "fd7a:115c:a1e0::1"},
		"HA.lan.example.":     {"100.64.0.9"},
		"api.example.com":     {"93.184.215.14"},
		"meta.mapped.example": {"::ffff:100.100.100.200"}, // the metadata address, IPv4-mapped: refused, not counted
	}
	resolve := func(_ context.Context, host string) ([]net.IP, error) {
		a, ok := dns[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var out []net.IP
		for _, s := range a {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
	got, err := agentCGNATConnections(context.Background(), []string{"hermes", "other"}, list, []dialguard.PrivateHost{{Host: "ha.lan.example"}}, resolve)
	if err != nil {
		t.Fatal(err)
	}
	want := api.AgentCGNATStatus{
		Examined:   7, // the removed row is not examined
		Resolved:   6,
		Unresolved: []string{"other/other-x/dark (nowhere.invalid)"},
		Found: []api.AgentCGNATConnection{
			{Namespace: "hermes", ProjectID: "hermes-home", Integration: "literal", Host: "100.100.1.2", Addrs: []string{"100.100.1.2"}},
			{Namespace: "hermes", ProjectID: "hermes-home", Integration: "nas", Host: "nas.tail1234.ts.net", Addrs: []string{"100.101.1.2"}},
			{Namespace: "hermes", ProjectID: "hermes-home", Integration: "ha", Host: "HA.lan.example.", Addrs: []string{"100.64.0.9"}, Listed: true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// The doctor run's wait is bounded (design review 2691 F5): a host shared by
// several rows is resolved once.
func TestAgentCGNATConnections_ResolvesEachHostOnce_893(t *testing.T) {
	rows := []persistence.AgentIntegrationApproval{
		{Namespace: "hermes", ProjectID: "a", Integration: "x", URL: "https://peer.ts.net/a"},
		{Namespace: "hermes", ProjectID: "b", Integration: "y", URL: "https://peer.ts.net/b"},
	}
	list := func(context.Context, string) ([]persistence.AgentIntegrationApproval, error) { return rows, nil }
	calls := 0
	resolve := func(context.Context, string) ([]net.IP, error) {
		calls++
		return []net.IP{net.ParseIP("100.90.0.1")}, nil
	}
	got, err := agentCGNATConnections(context.Background(), []string{"hermes"}, list, nil, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(got.Found) != 2 {
		t.Fatalf("resolved %d times, found %d; want 1 and 2", calls, len(got.Found))
	}
}

// Without the registry or the approval tables the source says so, and the
// check reports it as unread rather than clean.
func TestAgentCGNATStatus_NotWired_893(t *testing.T) {
	if _, err := (&Container{}).agentCGNATStatus(context.Background()); err == nil {
		t.Fatal("an unwired container reported a clean examination")
	}
}

// A namespace whose rows cannot be read fails the source, so the check says
// it could not read rather than reporting a clean examination.
func TestAgentCGNATConnections_ReadError_893(t *testing.T) {
	list := func(context.Context, string) ([]persistence.AgentIntegrationApproval, error) {
		return nil, errors.New("db down")
	}
	if _, err := agentCGNATConnections(context.Background(), []string{"hermes"}, list, nil, nil); err == nil {
		t.Fatal("a read error was swallowed")
	}
}
