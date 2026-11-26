package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/integrations/dialguard"
)

// GitHub #740 (2026-11-15; agent-administered design part 06, §6.8). Found at
// production by the same CE report as #734: an approved agent MCP server on
// an intranet host was refused at every dial, although the operator listed
// the host in agent_admin.private_api_hosts and test_connection's rule text
// said the list would allow it. Every agent-server dial now builds its client
// with the list, through one constructor.
//
// The test host has no private route, so the seam replaces the constructor
// (agentServerClientFor) with one that records what it was given and sends
// the request to a local fake; whether a granted host reaches a private
// address is covered by the guard's own tests (dialguard, #734 and #740).

const intranetMCPURL = "https://intranet.example/mcp"

type agentClientCall struct {
	url   string
	hosts []string
}

type agentClientRecorder struct {
	mu    sync.Mutex
	calls []agentClientCall
}

func (r *agentClientRecorder) snapshot() []agentClientCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentClientCall(nil), r.calls...)
}

// granted reports how many recorded clients were built for intranetMCPURL
// with the intranet host in their list.
func (r *agentClientRecorder) granted() int {
	n := 0
	for _, c := range r.snapshot() {
		if c.url == intranetMCPURL && slices.Contains(c.hosts, "intranet.example") {
			n++
		}
	}
	return n
}

// recordAgentServerClients replaces the constructor seam with one that
// records each call and returns a client whose every request goes to target.
func recordAgentServerClients(t *testing.T, target string) *agentClientRecorder {
	t.Helper()
	to, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	rec := &agentClientRecorder{}
	prev := agentServerClientFor
	agentServerClientFor = func(rawURL string, entries []dialguard.PrivateHost, timeout time.Duration) *http.Client {
		var hosts []string
		for _, e := range entries {
			hosts = append(hosts, e.HostPort())
		}
		rec.mu.Lock()
		rec.calls = append(rec.calls, agentClientCall{url: rawURL, hosts: hosts})
		rec.mu.Unlock()
		return &http.Client{Timeout: timeout, Transport: rewriteTransport{to: to}}
	}
	t.Cleanup(func() { agentServerClientFor = prev })
	return rec
}

// rewriteTransport sends every request to one local server.
type rewriteTransport struct{ to *url.URL }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme, r.URL.Host, r.Host = rt.to.Scheme, rt.to.Host, rt.to.Host
	return http.DefaultTransport.RoundTrip(r)
}

func intranetFixture(t *testing.T, hosts ...string) *agentAdminFixture {
	t.Helper()
	return newAgentAdminFixtureWith(t, func(c *config.Config) { c.AgentAdmin.PrivateAPIHosts = plainPrivateHosts(hosts...) })
}

// plainPrivateHosts is agent_admin.private_api_hosts as a plain string list
// loads (GitHub #403 made an item a string or a mapping).
func plainPrivateHosts(hosts ...string) []config.PrivateAPIHost {
	out := make([]config.PrivateAPIHost, len(hosts))
	for i, h := range hosts {
		out[i] = config.PrivateAPIHost{Host: h}
	}
	return out
}

func TestAgentMCP_740_EveryDialPassesThePrivateHosts(t *testing.T) {
	fake := newFakeMCP(t, "", "read_doc")
	rec := recordAgentServerClients(t, fake.srv.URL)
	f := intranetFixture(t, "intranet.example")

	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "lan", Purpose: "Intranet docs"})
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "lan", Name: "intranet", URL: intranetMCPURL})
	if res.Effect != agentadmin.EffectAwaiting || !strings.Contains(res.Sentence, "read_doc") {
		t.Fatalf("the pre-approval listing did not reach the server through the granted client: %+v", res)
	}
	if rec.granted() == 0 {
		t.Fatalf("the pre-approval listing built no client with the list: %+v", rec.snapshot())
	}
	f.approve(res)

	before := rec.granted()
	wired := f.c.mcpDesiredServers()["hermes--lan"]
	if len(wired) != 1 || wired[0].HTTPClient == nil || rec.granted() <= before {
		t.Fatalf("the role-call config was not built with the list: %+v, calls %+v", wired, rec.snapshot())
	}

	before = rec.granted()
	if got := f.mustTestConn("lan", "intranet"); got.Code != agentadmin.ConnOK {
		t.Fatalf("test_connection of the approved intranet server: %+v", got)
	}
	if rec.granted() <= before {
		t.Fatalf("test_connection built no client with the list: %+v", rec.snapshot())
	}

	before = rec.granted()
	if f.c.agentOAuthHTTP("hermes--lan", intranetMCPURL) == nil || rec.granted() <= before {
		t.Fatalf("the OAuth client was not built with the list: %+v", rec.snapshot())
	}
}

// §6.8 item 5: before approval the agent picks the path, so a failed listing
// of a URL the grant admits tells it nothing a probe could use.
func TestAgentMCP_740_GrantedPreApprovalFailureIsUniform(t *testing.T) {
	notFound := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notFound.Close)
	recordAgentServerClients(t, notFound.URL)

	f := intranetFixture(t, "intranet.example")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "lan", Purpose: "Intranet docs"})
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "lan", Name: "intranet", URL: intranetMCPURL})
	if res.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("a granted host's failed listing must be filed, not refused with a hint: %+v", res)
	}
	for _, leak := range []string{"404", "HTTP 4", "did not answer", "refused", "timed out"} {
		if strings.Contains(res.Sentence, leak) {
			t.Fatalf("the sentence tells the agent %q: %s", leak, res.Sentence)
		}
	}

	// Without the grant the same answer is a public host's: its class shows.
	g := intranetFixture(t)
	g.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "lan", Purpose: "Intranet docs"})
	res = g.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "lan", Name: "intranet", URL: intranetMCPURL})
	if res.Effect != agentadmin.EffectRefused || !strings.Contains(res.Reason+res.Sentence, "initialize returned 404") {
		t.Fatalf("without the grant a 404 should give the transport hint: %+v", res)
	}
}

// The grant is read at the dial, not cached at boot: the list is the one the
// container's configuration holds.
func TestAgentMCP_740_NoListNoGrant(t *testing.T) {
	fake := newFakeMCP(t, "", "read_doc")
	rec := recordAgentServerClients(t, fake.srv.URL)
	c := &Container{}
	if _, err := c.listMCPTools(context.Background(), mcpServerConfigFor("hermes", intranetMCPURL)); err != nil {
		t.Fatal(err)
	}
	for _, call := range rec.snapshot() {
		if len(call.hosts) != 0 {
			t.Fatalf("a container with no list granted %v", call.hosts)
		}
	}
}
