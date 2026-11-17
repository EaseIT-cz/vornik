package dialguard

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// FoldHost is the form agent_admin.private_api_hosts entries and an agent
// server's host are compared in: lowercase, no trailing dot (agent-administered
// design part 06, §6.7, GitHub #734).
func FoldHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

// PrivateHostGranted reports whether agent_admin.private_api_hosts admits
// rawURL: an https URL whose folded host equals a folded entry that
// ValidModelHost accepts, and never under a managed policy (fail closed; the
// config loader refuses the list there too). It is the one statement of the
// grant (§6.8, GitHub #740): the guard and the pre-approval listing's mask
// both read it.
func PrivateHostGranted(rawURL string, privateHosts []string) bool {
	_, ok := grantedHost(rawURL, privateHosts)
	return ok
}

// grantedHost is PrivateHostGranted with the URL's host as written, parsed
// once for both the predicate and the guard (code review 4b02 F3).
func grantedHost(rawURL string, privateHosts []string) (string, bool) {
	if currentManagedPolicy() != nil || len(privateHosts) == 0 {
		return "", false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", false
	}
	host := u.Hostname()
	for _, h := range privateHosts {
		// Fail closed on an entry the loader would refuse (an IP literal, a
		// wildcard), whoever built the list (code review 91fb F1).
		if ValidModelHost(h) == nil && FoldHost(h) == FoldHost(host) {
			return host, true
		}
	}
	return "", false
}

// AgentURLGuard is the guard for an agent project's approved API or MCP
// server (§6.7, §6.8). It is CustomerURLGuard(rawURL, nil), plus, when
// PrivateHostGranted, PrivateHosts for the URL's host: it may then resolve to
// a private address, never to loopback, link-local or a metadata address.
func AgentURLGuard(rawURL string, privateHosts []string) DialGuard {
	g := CustomerURLGuard(rawURL, nil)
	if host, ok := grantedHost(rawURL, privateHosts); ok {
		// The dial sees the host as the transport spells it; every spelling
		// of this one host is granted.
		g.PrivateHosts = []string{host, FoldHost(host), strings.ToLower(host)}
	}
	return g
}

// AgentURLClient is CustomerURLClient with AgentURLGuard as its guard, read
// on every dial. agentapi.Client refuses every redirect on top of it; an MCP
// client keeps CustomerURLClient's same-host redirects.
func AgentURLClient(rawURL string, privateHosts []string, timeout time.Duration) *http.Client {
	hosts := append([]string(nil), privateHosts...)
	c := CustomerURLClient(rawURL, nil, timeout)
	c.Transport = liveClient(timeout, func() DialGuard { return AgentURLGuard(rawURL, hosts) }).Transport
	return c
}
