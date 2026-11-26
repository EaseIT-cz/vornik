package dialguard

import (
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// GitHub #403 (agent-administered design part 06, §6.12): the operator's
// answers. An agent_admin.private_api_hosts entry may be an IP literal or
// host:port, may pin the addresses its name may reach, and may serve named
// projects only. Each test drives the guard's Control with the address a
// resolver would return, as agent_url_rebind_403_test.go does: the test host
// has no private route.

func mustEntry(t *testing.T, s string) PrivateHost {
	t.Helper()
	e, err := ParsePrivateHost(s)
	if err != nil {
		t.Fatalf("ParsePrivateHost(%q): %v", s, err)
	}
	return e
}

func admits(t *testing.T, g DialGuard, host, addr string) bool {
	t.Helper()
	err := g.Dialer(host).Control("tcp", addr, nil)
	if err != nil && !errors.Is(err, ErrDialRefused) {
		t.Fatalf("%s: unexpected error %v", addr, err)
	}
	return err == nil
}

func TestParsePrivateHost_403_Forms(t *testing.T) {
	for in, want := range map[string]string{
		"GitLab.Lan.":        "gitlab.lan",
		"nas.lan:8443":       "nas.lan:8443",
		"10.0.0.5":           "10.0.0.5",
		"10.0.0.5:8443":      "10.0.0.5:8443",
		"100.101.1.2":        "100.101.1.2",
		"fd00:0::5":          "fd00::5",
		"[fd00::5]:8443":     "[fd00::5]:8443",
		"::ffff:192.168.1.9": "192.168.1.9",
		"198.51.100.7":       "198.51.100.7",
	} {
		e, err := ParsePrivateHost(in)
		if err != nil {
			t.Errorf("%q refused: %v", in, err)
			continue
		}
		if got := e.String(); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{
		"", "127.0.0.1", "::1", "169.254.169.254", "fe80::1", "fe80::1%eth0", "fd00:ec2::254", "100.100.100.200",
		"0.0.0.0", "224.0.0.1", "255.255.255.255", "64:ff9b::a00:5", "2002:a00:5::", "::a00:5",
		"nas.lan:0", "nas.lan:65536", "nas.lan:x", "fd00::5:8443x", "https://nas.lan", "*.lan", "nas.lan/x", "[nas.lan]:443",
		"localhost",
	} {
		if _, err := ParsePrivateHost(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParsePin_403(t *testing.T) {
	for in, want := range map[string]string{"10.0.0.0/24": "10.0.0.0/24", "10.0.1.7": "10.0.1.7", "10.0.0.9/24": "10.0.0.0/24", "fd00::/8": "fd00::/8"} {
		p, err := ParsePin(in)
		if err != nil {
			t.Errorf("%q refused: %v", in, err)
			continue
		}
		if got := pinString(p); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
	// A pin that could only ever refuse is a mistake (review b6d3 F6).
	for _, bad := range []string{"127.0.0.0/8", "169.254.0.0/16", "0.0.0.0/0", "224.0.0.0/4", "169.254.169.254", "64:ff9b::/96", "x", ""} {
		if _, err := ParsePin(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPrivateHostGrant_403_PortAndLiteral(t *testing.T) {
	entries := []PrivateHost{mustEntry(t, "nas.lan:8443"), mustEntry(t, "fd00::5"), mustEntry(t, "10.0.0.5")}
	for u, want := range map[string]bool{
		"https://nas.lan:8443/mcp":   true,
		"https://NAS.lan.:8443/mcp":  true,
		"https://nas.lan/mcp":        false, // 443, not the entry's port
		"https://nas.lan:9000/mcp":   false,
		"http://nas.lan:8443/mcp":    false,
		"https://[fd00:0::5]/":       true, // canonical literal
		"https://[fd00::5]:8443/":    true, // no port on the entry: any URL port
		"https://10.0.0.5/":          true,
		"https://[::ffff:10.0.0.5]/": true,
		"https://10.0.0.6/":          false,
		"https://[fe80::1%25eth0]/":  false,
		"https://other.lan:8443/mcp": false,
	} {
		if got := PrivateHostGranted(u, entries); got != want {
			t.Errorf("%s: granted %v, want %v", u, got, want)
		}
	}
}

// A port entry grants the private reach on that port only, at the dial: a
// request the same client sends to another port of the host (a redirect)
// reaches public addresses only.
func TestAgentURLGuard_403_PortIsCheckedAtTheDial(t *testing.T) {
	g := AgentURLGuard("https://nas.lan:8443/mcp", []PrivateHost{mustEntry(t, "nas.lan:8443")})
	if !admits(t, g, "nas.lan", "10.0.0.5:8443") {
		t.Error("10.0.0.5:8443 refused")
	}
	if admits(t, g, "nas.lan", "10.0.0.5:443") || admits(t, g, "nas.lan", "10.0.0.5:80") {
		t.Error("another port of the host got the private grant")
	}
	// A plain entry grants the URL's own port, not every port (§6.12 item 3).
	g = AgentURLGuard("https://gitlab.lan/api", []PrivateHost{mustEntry(t, "gitlab.lan")})
	if !admits(t, g, "gitlab.lan", "10.0.0.7:443") || admits(t, g, "gitlab.lan", "10.0.0.7:80") {
		t.Error("a plain entry's grant is not bound to the URL's port")
	}
}

func TestAgentURLGuard_403_PinsBindEveryDialOfTheHost(t *testing.T) {
	e := mustEntry(t, "wiki.lan")
	pin, _ := ParsePin("10.0.0.0/24")
	e.Addresses = []netip.Prefix{pin}
	g := AgentURLGuard("https://wiki.lan/", []PrivateHost{e})
	if !admits(t, g, "wiki.lan", "10.0.0.9:443") {
		t.Error("an address inside the pin refused")
	}
	for _, addr := range []string{"10.0.1.9:443", "198.51.100.7:443", "127.0.0.1:443", "169.254.169.254:443", "10.0.0.9:80"} {
		if admits(t, g, "wiki.lan", addr) {
			t.Errorf("%s admitted outside the pin or the rules", addr)
		}
	}
	// Unpinned, the name keeps by-name trust: any private address, and a
	// public one.
	g = AgentURLGuard("https://wiki.lan/", []PrivateHost{mustEntry(t, "wiki.lan")})
	for _, addr := range []string{"10.0.1.9:443", "198.51.100.7:443", "100.101.1.2:443"} {
		if !admits(t, g, "wiki.lan", addr) {
			t.Errorf("%s refused without pins", addr)
		}
	}
}

func TestAgentURLGuard_403_LiteralEntriesDial(t *testing.T) {
	for _, c := range []struct{ entry, url, host, addr string }{
		{"10.0.0.5", "https://10.0.0.5/", "10.0.0.5", "10.0.0.5:443"},
		{"100.101.1.2", "https://100.101.1.2/", "100.101.1.2", "100.101.1.2:443"},
		{"fd00::5", "https://[fd00::5]/", "fd00::5", "[fd00::5]:443"},
		{"[fd00::5]:8443", "https://[fd00::5]:8443/", "fd00::5", "[fd00::5]:8443"},
	} {
		g := AgentURLGuard(c.url, []PrivateHost{mustEntry(t, c.entry)})
		if !admits(t, g, c.host, c.addr) {
			t.Errorf("%s: %s refused", c.entry, c.addr)
		}
		if admits(t, AgentURLGuard(c.url, nil), c.host, c.addr) {
			t.Errorf("%s: admitted with no entry", c.url)
		}
	}
}

// Two entries for one host: the first that serves the project is the grant
// (review b6d3 F5, F7b, F7c).
func TestPrivateHostGrant_403_FirstMatchWins(t *testing.T) {
	pinned := mustEntry(t, "wiki.lan")
	pin, _ := ParsePin("10.0.0.0/24")
	pinned.Addresses = []netip.Prefix{pin}
	plain := mustEntry(t, "wiki.lan")
	if g := AgentURLGuard("https://wiki.lan/", []PrivateHost{pinned, plain}); admits(t, g, "wiki.lan", "10.0.1.9:443") {
		t.Error("pinned first: 10.0.1.9 admitted")
	}
	if g := AgentURLGuard("https://wiki.lan/", []PrivateHost{plain, pinned}); !admits(t, g, "wiki.lan", "10.0.1.9:443") {
		t.Error("plain first: 10.0.1.9 refused")
	}
	scoped := pinned
	scoped.Projects = []string{"hermes--home"}
	all := []PrivateHost{scoped, plain}
	if e, _ := PrivateHostGrant("https://wiki.lan/", PrivateHostsFor("hermes--home", all)); len(e.Addresses) != 1 {
		t.Errorf("hermes--home got %v, want the scoped pinned entry", e)
	}
	if e, _ := PrivateHostGrant("https://wiki.lan/", PrivateHostsFor("hermes--other", all)); len(e.Addresses) != 0 {
		t.Errorf("hermes--other got %v, want the plain entry", e)
	}
	if got := PrivateHostsFor("hermes--other", []PrivateHost{scoped}); len(got) != 0 {
		t.Errorf("an entry scoped to another project serves: %v", got)
	}
}

// A granted client follows only same-origin redirects, self-hosted too
// (§6.12 item 4): before, a listed host could redirect to http:// on itself
// or to another port and keep the grant.
func TestAgentURLClient_403_GrantedClientKeepsTheOrigin(t *testing.T) {
	c := AgentURLClient("https://nas.lan:8443/mcp", []PrivateHost{mustEntry(t, "nas.lan")}, 0)
	from, _ := http.NewRequest(http.MethodGet, "https://nas.lan:8443/mcp", nil)
	for _, to := range []string{"http://nas.lan:8443/x", "https://nas.lan:9000/x", "https://other.lan:8443/x"} {
		u, _ := url.Parse(to)
		if err := c.CheckRedirect(&http.Request{URL: u}, []*http.Request{from}); err == nil {
			t.Errorf("redirect to %s followed", to)
		}
	}
	u, _ := url.Parse("https://nas.lan:8443/other")
	if err := c.CheckRedirect(&http.Request{URL: u}, []*http.Request{from}); err != nil {
		t.Errorf("same-origin redirect refused: %v", err)
	}
	// Pins bind the resolved address of a redirect too: the guard is the
	// same for every request the client sends (review b6d3 F7d).
	e := mustEntry(t, "nas.lan")
	pin, _ := ParsePin("10.0.0.0/24")
	e.Addresses = []netip.Prefix{pin}
	if admits(t, AgentURLGuard("https://nas.lan:8443/mcp", []PrivateHost{e}), "nas.lan", "10.9.9.9:8443") {
		t.Error("a dial outside the pins admitted")
	}
	// An unlisted URL's client is unchanged: self-hosted, any redirect.
	plain := AgentURLClient("https://pub.example/mcp", nil, 0)
	pu, _ := url.Parse("https://elsewhere.example/")
	pf, _ := http.NewRequest(http.MethodGet, "https://pub.example/mcp", nil)
	if err := plain.CheckRedirect(&http.Request{URL: pu}, []*http.Request{pf}); err != nil {
		t.Errorf("an unlisted client's redirect rule changed: %v", err)
	}
}

func TestPrivateHostString_403(t *testing.T) {
	e := mustEntry(t, "nas.lan:8443")
	a, _ := ParsePin("10.0.0.0/24")
	b, _ := ParsePin("10.0.1.7")
	e.Addresses = []netip.Prefix{a, b}
	if got := e.String(); got != "nas.lan:8443 within 10.0.0.0/24, 10.0.1.7" {
		t.Errorf("String = %q", got)
	}
	if got := e.HostPort(); got != "nas.lan:8443" || strings.Contains(e.HostPort(), "within") {
		t.Errorf("HostPort = %q", got)
	}
}

// Code review 3a8c F1: AllowedHosts and ExactAddrs skip the address check
// before Control runs, so a granted guard must carry neither, or a pinned
// name could be dialled outside its pins.
func TestAgentURLGuard_403_GrantedGuardHasNoBypass(t *testing.T) {
	e := mustEntry(t, "10.0.0.5:8443")
	for _, u := range []string{"https://10.0.0.5:8443/", "https://wiki.lan/"} {
		g := AgentURLGuard(u, []PrivateHost{e, mustEntry(t, "wiki.lan")})
		if len(g.PrivateHosts) == 0 || len(g.AllowedHosts) != 0 || len(g.ExactAddrs) != 0 {
			t.Errorf("%s: want a grant with no bypass: %+v", u, g)
		}
	}
}
