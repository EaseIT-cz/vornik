package dialguard

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// FoldHost is the form agent_admin.private_api_hosts entries and an API's
// host are compared in: lowercase, no trailing dot (agent-administered
// design part 06, §6.7, GitHub #734).
func FoldHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

// AgentAPIGuard is the guard for an agent project's approved API (§6.7). It
// is CustomerURLGuard(rawURL, nil), plus, when the base URL is https and its
// host (folded) is one of privateHosts (folded), PrivateHosts for that host:
// it may then resolve to a private address, never to loopback, link-local or
// a metadata address. Under a managed policy the list is ignored (fail
// closed; the config loader refuses it there too).
func AgentAPIGuard(rawURL string, privateHosts []string) DialGuard {
	g := CustomerURLGuard(rawURL, nil)
	if currentManagedPolicy() != nil || len(privateHosts) == 0 {
		return g
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return g
	}
	host := u.Hostname()
	for _, h := range privateHosts {
		// Fail closed on an entry the loader would refuse (an IP literal, a
		// wildcard), whoever built the list (code review 91fb F1).
		if ValidModelHost(h) != nil {
			continue
		}
		if FoldHost(h) == FoldHost(host) {
			// The dial sees the host as the transport spells it; every
			// spelling of this one host is granted.
			g.PrivateHosts = []string{host, FoldHost(host), strings.ToLower(host)}
			return g
		}
	}
	return g
}

// AgentAPIClient is CustomerURLClient with AgentAPIGuard as its guard, read
// on every dial. agentapi.Client refuses every redirect on top of it.
func AgentAPIClient(rawURL string, privateHosts []string, timeout time.Duration) *http.Client {
	hosts := append([]string(nil), privateHosts...)
	c := CustomerURLClient(rawURL, nil, timeout)
	c.Transport = liveClient(timeout, func() DialGuard { return AgentAPIGuard(rawURL, hosts) }).Transport
	return c
}
