package service

import (
	"context"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #403 (agent-administered design part 06, §6.12 items 8 and 9): the
// doctor's source for unused agent_admin.private_api_hosts entries, and
// describe_installation's intranet_hosts.

func TestUnusedPrivateHosts_403(t *testing.T) {
	removed := time.Now()
	rows := map[string][]persistence.AgentIntegrationApproval{
		"hermes": {
			{Namespace: "hermes", ProjectID: "hermes--home", Integration: "nas", URL: "https://nas.lan:8443/mcp"},
			{Namespace: "hermes", ProjectID: "hermes--home", Integration: "old", URL: "https://old.lan/", RemovedAt: &removed},
			{Namespace: "hermes", ProjectID: "hermes--other", Integration: "wiki", URL: "https://wiki.lan/"},
		},
	}
	list := func(_ context.Context, ns string) ([]persistence.AgentIntegrationApproval, error) {
		return rows[ns], nil
	}
	entry := func(h string, projects ...string) dialguard.PrivateHost {
		e, err := dialguard.ParsePrivateHost(h)
		if err != nil {
			t.Fatal(err)
		}
		e.Projects = projects
		return e
	}
	entries := []dialguard.PrivateHost{
		entry("nas.lan:8443"),             // used by hermes--home/nas
		entry("old.lan"),                  // only a removed connection
		entry("wiki.lan", "hermes--home"), // wiki is in hermes--other: unused
		entry("wiki.lan"),                 // used by hermes--other/wiki
		entry("nas.lan"),                  // shadowed by the first entry: unused
	}
	got, err := unusedPrivateHosts(context.Background(), []string{"hermes"}, list, entries)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"old.lan", "wiki.lan (projects hermes--home)", "nas.lan"}
	if got.Entries != 5 || got.Connections != 2 || !reflect.DeepEqual(got.Unused, want) {
		t.Fatalf("got %+v, want 5 entries, 2 connections, unused %v", got, want)
	}
}

func TestIntranetHostsFor_403(t *testing.T) {
	pin, _ := dialguard.ParsePin("10.0.0.0/24")
	entries := []dialguard.PrivateHost{
		{Host: "gitlab.lan"},
		{Host: "nas.lan", Port: "8443", Projects: []string{"hermes--home", "claude--lab"}},
		{Host: "wiki.lan", Projects: []string{"claude--lab"}},
		{Host: "files.lan", Addresses: []netip.Prefix{pin}},
	}
	got := intranetHostsFor("hermes", entries)
	want := []string{"gitlab.lan (every project)", "nas.lan:8443 (projects hermes--home)", "files.lan (every project)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := intranetHostsFor("hermes", nil); got == nil || len(got) != 0 {
		t.Fatalf("no entries: %v, want an empty list", got)
	}
}
