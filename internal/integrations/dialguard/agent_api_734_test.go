package dialguard

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// GitHub #734 (agent-administered design part 06, §6.7). Found at production
// (a CE report): an agent project's approved API on an intranet https host
// was never reachable, because every agent-API dial refused private
// addresses and nothing could exempt one. agent_admin.private_api_hosts now
// lets the operator name such hosts; a listed https host may reach a private
// address and nothing else the guard refuses.

func control(t *testing.T, g DialGuard, host, addr string) error {
	t.Helper()
	return g.Dialer(host).Control("tcp", addr, nil)
}

func TestAgentURLGuard_734_ListedHTTPSHostReachesPrivateOnly(t *testing.T) {
	g := AgentURLGuard("https://gitlab.corp.example/api/v4", names("gitlab.corp.example"))
	for _, addr := range []string{"10.0.0.5:443", "192.168.1.20:443", "[fd00::1]:443", "198.51.100.7:443"} {
		if err := control(t, g, "gitlab.corp.example", addr); err != nil {
			t.Errorf("listed https host refused %s: %v", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:443", "[::1]:443", "169.254.169.254:80", "[fe80::1]:443", "0.0.0.0:443", "[64:ff9b::a9fe:a9fe]:80"} {
		if err := control(t, g, "gitlab.corp.example", addr); !errors.Is(err, ErrDialRefused) {
			t.Errorf("listed https host dialled %s: %v (a listed name never reaches loopback, link-local or metadata)", addr, err)
		}
	}
}

func TestAgentURLGuard_734_UnlistedOrPlainHTTPGetsNoGrant(t *testing.T) {
	for name, g := range map[string]DialGuard{
		"unlisted host":     AgentURLGuard("https://other.corp.example/", names("gitlab.corp.example")),
		"listed host, http": AgentURLGuard("http://gitlab.corp.example/", names("gitlab.corp.example")),
		"empty list":        AgentURLGuard("https://gitlab.corp.example/", nil),
		"unparsable base":   AgentURLGuard("://gitlab.corp.example", names("gitlab.corp.example")),
	} {
		host := "gitlab.corp.example"
		if name == "unlisted host" {
			host = "other.corp.example"
		}
		if err := control(t, g, host, "10.0.0.5:443"); !errors.Is(err, ErrDialRefused) {
			t.Errorf("%s: dialled a private address: %v", name, err)
		}
	}
}

func TestAgentURLGuard_734_EntriesAndURLAreFolded(t *testing.T) {
	g := AgentURLGuard("https://GitLab.Corp.Example./x", names("gitlab.corp.example"))
	if err := control(t, g, "GitLab.Corp.Example.", "10.0.0.5:443"); err != nil {
		t.Errorf("a host differing only in case and a trailing dot got no grant: %v", err)
	}
	g = AgentURLGuard("https://gitlab.corp.example/x", names("GITLAB.corp.example."))
	if err := control(t, g, "gitlab.corp.example", "10.0.0.5:443"); err != nil {
		t.Errorf("an unfolded entry did not match: %v", err)
	}
	if got := FoldHost("Intranet.Example."); got != "intranet.example" {
		t.Errorf("FoldHost = %q", got)
	}
}

func TestAgentURLGuard_734_ManagedPolicyIgnoresTheList(t *testing.T) {
	withPolicy(t, &ManagedPolicy{})
	g := AgentURLGuard("https://gitlab.corp.example/", names("gitlab.corp.example"))
	if err := control(t, g, "gitlab.corp.example", "10.0.0.5:443"); !errors.Is(err, ErrDialRefused) {
		t.Errorf("under a managed policy a listed host dialled a private address: %v", err)
	}
	if AgentURLClient("https://gitlab.corp.example/", names("gitlab.corp.example"), 0) == nil {
		t.Fatal("AgentURLClient returned nil")
	}
}

// Code review 91fb F1 and F2: an entry the loader would refuse never
// grants (an IP literal was one until #403 made it a valid entry, §6.12; a
// link-local literal still is), and the client keeps CustomerURLClient's
// no-proxy transport and redirect rule.
func TestAgentURLClient_734_KeepsTheGuardsShape(t *testing.T) {
	g := AgentURLGuard("https://169.254.1.1/", names("169.254.1.1"))
	if err := control(t, g, "169.254.1.1", "169.254.1.1:443"); !errors.Is(err, ErrDialRefused) {
		t.Errorf("a link-local-literal entry granted: %v", err)
	}
	c := AgentURLClient("https://gitlab.corp.example/", names("gitlab.corp.example"), time.Second)
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || tr.DialContext == nil {
		t.Fatalf("transport %T: want a guarded transport that consults no proxy", c.Transport)
	}
	if c.CheckRedirect == nil {
		t.Fatal("the redirect rule was dropped")
	}
}
