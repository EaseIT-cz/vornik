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

func TestAgentAPIGuard_734_ListedHTTPSHostReachesPrivateOnly(t *testing.T) {
	g := AgentAPIGuard("https://gitlab.corp.example/api/v4", []string{"gitlab.corp.example"})
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

func TestAgentAPIGuard_734_UnlistedOrPlainHTTPGetsNoGrant(t *testing.T) {
	for name, g := range map[string]DialGuard{
		"unlisted host":     AgentAPIGuard("https://other.corp.example/", []string{"gitlab.corp.example"}),
		"listed host, http": AgentAPIGuard("http://gitlab.corp.example/", []string{"gitlab.corp.example"}),
		"empty list":        AgentAPIGuard("https://gitlab.corp.example/", nil),
		"unparsable base":   AgentAPIGuard("://gitlab.corp.example", []string{"gitlab.corp.example"}),
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

func TestAgentAPIGuard_734_EntriesAndURLAreFolded(t *testing.T) {
	g := AgentAPIGuard("https://GitLab.Corp.Example./x", []string{"gitlab.corp.example"})
	if err := control(t, g, "GitLab.Corp.Example.", "10.0.0.5:443"); err != nil {
		t.Errorf("a host differing only in case and a trailing dot got no grant: %v", err)
	}
	g = AgentAPIGuard("https://gitlab.corp.example/x", []string{"GITLAB.corp.example."})
	if err := control(t, g, "gitlab.corp.example", "10.0.0.5:443"); err != nil {
		t.Errorf("an unfolded entry did not match: %v", err)
	}
	if got := FoldHost("Intranet.Example."); got != "intranet.example" {
		t.Errorf("FoldHost = %q", got)
	}
}

func TestAgentAPIGuard_734_ManagedPolicyIgnoresTheList(t *testing.T) {
	withPolicy(t, &ManagedPolicy{})
	g := AgentAPIGuard("https://gitlab.corp.example/", []string{"gitlab.corp.example"})
	if err := control(t, g, "gitlab.corp.example", "10.0.0.5:443"); !errors.Is(err, ErrDialRefused) {
		t.Errorf("under a managed policy a listed host dialled a private address: %v", err)
	}
	if AgentAPIClient("https://gitlab.corp.example/", []string{"gitlab.corp.example"}, 0) == nil {
		t.Fatal("AgentAPIClient returned nil")
	}
}

// Code review 91fb F1 and F2: an IP-literal entry never grants, and the
// client keeps CustomerURLClient's no-proxy transport and redirect rule.
func TestAgentAPIClient_734_KeepsTheGuardsShape(t *testing.T) {
	g := AgentAPIGuard("https://10.0.0.5/", []string{"10.0.0.5"})
	if err := control(t, g, "10.0.0.5", "10.0.0.5:443"); !errors.Is(err, ErrDialRefused) {
		t.Errorf("an IP-literal entry granted a private address: %v", err)
	}
	c := AgentAPIClient("https://gitlab.corp.example/", []string{"gitlab.corp.example"}, time.Second)
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || tr.DialContext == nil {
		t.Fatalf("transport %T: want a guarded transport that consults no proxy", c.Transport)
	}
	if c.CheckRedirect == nil {
		t.Fatal("the redirect rule was dropped")
	}
}
