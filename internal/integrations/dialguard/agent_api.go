package dialguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// FoldHost is the form agent_admin.private_api_hosts entries and an agent
// server's host are compared in: lowercase, no trailing dot (agent-administered
// design part 06, §6.7, GitHub #734).
func FoldHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

// PrivateHost is one agent_admin.private_api_hosts entry, parsed (part 06
// §6.12, GitHub #403). Host is a folded name or a canonical IP literal; Port
// is "" for any URL port; Addresses, when set, pin every dial of the granted
// host; Projects, when set, are the agent projects the entry serves (empty:
// every agent project).
type PrivateHost struct {
	Host      string
	Port      string
	Addresses []netip.Prefix
	Projects  []string
}

// ParsePrivateHost parses the host form of an entry: a name, an IP literal,
// or either with ":port" (an IPv6 literal with a port in brackets). An
// IPv4-mapped literal is unmapped to its IPv4 address first (code review
// dd02 F5). A literal the grant could never admit (loopback, link-local,
// multicast, unspecified, a metadata address, a NAT64, 6to4 or
// IPv4-compatible form) is refused: listing it is a mistake.
func ParsePrivateHost(s string) (PrivateHost, error) {
	host, port := s, ""
	switch {
	case strings.HasPrefix(s, "["):
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return PrivateHost{}, fmt.Errorf("%q: %v", s, err)
		}
		if _, err := netip.ParseAddr(h); err != nil || !strings.Contains(h, ":") {
			return PrivateHost{}, fmt.Errorf("%q: brackets enclose an IPv6 address only", s)
		}
		host, port = h, p
	case strings.Count(s, ":") == 1:
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return PrivateHost{}, fmt.Errorf("%q: %v", s, err)
		}
		host, port = h, p
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return PrivateHost{}, fmt.Errorf("%q: the port must be 1 to 65535", s)
		}
	}
	h, err := canonicalEntryHost(host)
	if err != nil {
		return PrivateHost{}, err
	}
	return PrivateHost{Host: h, Port: port}, nil
}

// canonicalEntryHost is an entry host's compared form: a folded name, or a
// canonical, unmapped IP literal the grant could admit.
func canonicalEntryHost(h string) (string, error) {
	if a, err := netip.ParseAddr(h); err == nil {
		if a.Zone() != "" {
			return "", fmt.Errorf("%q: an address with a zone cannot be listed", h)
		}
		a = a.Unmap()
		if !grantableAddr(a) {
			return "", fmt.Errorf("%q: loopback, link-local, multicast, unspecified, metadata and embedded addresses cannot be listed", h)
		}
		return a.String(), nil
	}
	if strings.EqualFold(h, "localhost") {
		return "", fmt.Errorf("%q names this machine", h)
	}
	if err := ValidModelHost(h); err != nil {
		return "", err
	}
	return FoldHost(h), nil
}

// grantableAddr reports whether a private_api_hosts grant could ever admit a:
// a public address, or a private one (RFC 1918, ULA, CGNAT) that is not
// loopback or a metadata address. The rule for a literal entry and a pin.
func grantableAddr(a netip.Addr) bool {
	ip := net.IP(a.Unmap().AsSlice())
	if isCloudMetadata(ip) || ip.IsLoopback() {
		return false
	}
	return !isBlockedIP(ip) || isGrantablePrivate(ip)
}

// ParsePin parses one entry address: a CIDR or a single address, masked.
// A pin whose network address the grant could never admit is refused
// (review b6d3 F6).
func ParsePin(s string) (netip.Prefix, error) {
	var p netip.Prefix
	if a, err := netip.ParseAddr(s); err == nil {
		a = a.Unmap()
		p = netip.PrefixFrom(a, a.BitLen())
	} else if p, err = netip.ParsePrefix(s); err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not an address or CIDR", s)
	}
	p = p.Masked()
	if p.Addr().Zone() != "" || !grantableAddr(p.Addr()) {
		return netip.Prefix{}, fmt.Errorf("%q: a pin of loopback, link-local, multicast, unspecified, metadata or embedded addresses could never admit a dial", s)
	}
	return p, nil
}

func pinString(p netip.Prefix) string {
	if p.IsSingleIP() {
		return p.Addr().String()
	}
	return p.String()
}

// HostPort is the entry's host, with its port when it has one.
func (e PrivateHost) HostPort() string {
	if e.Port == "" {
		return e.Host // a bare IPv6 literal takes no brackets
	}
	return net.JoinHostPort(e.Host, e.Port)
}

// String is the entry as an approval records it (IntegrationGrant.PrivateReach)
// and the sentence names it: host[:port], then " within a, b" when pinned.
func (e PrivateHost) String() string {
	if len(e.Addresses) == 0 {
		return e.HostPort()
	}
	return e.HostPort() + " within " + e.Pins()
}

// Pins is the entry's pinned addresses, comma-separated; "" when unpinned.
func (e PrivateHost) Pins() string {
	pins := make([]string, len(e.Addresses))
	for i, p := range e.Addresses {
		pins[i] = pinString(p)
	}
	return strings.Join(pins, ", ")
}

// Serves reports whether the entry serves the agent project.
func (e PrivateHost) Serves(project string) bool {
	if len(e.Projects) == 0 {
		return true
	}
	for _, p := range e.Projects {
		if p == project {
			return true
		}
	}
	return false
}

// PrivateHostsFor is the entries that serve project, in file order: the one
// per-project scope (§6.12 answer 5). Every grant is computed from it.
func PrivateHostsFor(project string, entries []PrivateHost) []PrivateHost {
	var out []PrivateHost
	for _, e := range entries {
		if e.Serves(project) {
			out = append(out, e)
		}
	}
	return out
}

// PrivateHostGranted reports whether one of entries admits rawURL
// (PrivateHostGrant). It is the one statement of the grant (§6.8, GitHub
// #740): the guard, the pre-approval listing's mask and the approval
// sentence all read it.
func PrivateHostGranted(rawURL string, entries []PrivateHost) bool {
	_, ok := PrivateHostGrant(rawURL, entries)
	return ok
}

// PrivateHostGrant is the first entry, in order, that admits rawURL: an https
// URL whose host, folded or canonicalised, equals the entry's, on the entry's
// port when it has one. Never under a managed policy (fail closed; the config
// loader refuses the list there too). The caller scopes entries to the
// project (PrivateHostsFor).
func PrivateHostGrant(rawURL string, entries []PrivateHost) (PrivateHost, bool) {
	e, _, ok := grantOf(rawURL, entries)
	return e, ok
}

// PrivateHostMatches reports whether e admits rawURL, ignoring its projects:
// the doctor's test of which entry granted a connection.
func PrivateHostMatches(rawURL string, e PrivateHost) bool {
	_, _, ok := grantOf(rawURL, []PrivateHost{e})
	return ok
}

// grantOf is PrivateHostGrant with the URL's host as written, parsed once for
// both the predicate and the guard (code review 4b02 F3).
func grantOf(rawURL string, entries []PrivateHost) (PrivateHost, string, bool) {
	if currentManagedPolicy() != nil || len(entries) == 0 {
		return PrivateHost{}, "", false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return PrivateHost{}, "", false
	}
	host := u.Hostname()
	want, err := urlHostForm(host)
	if err != nil {
		return PrivateHost{}, "", false
	}
	port := effectivePort(u)
	for _, e := range entries {
		// Fail closed on an entry the loader would refuse, whoever built
		// the list (code review 91fb F1).
		eh, err := canonicalEntryHost(e.Host)
		if err != nil || eh != want {
			continue
		}
		if e.Port != "" && e.Port != port {
			continue
		}
		return e, host, true
	}
	return PrivateHost{}, "", false
}

// urlHostForm is a URL host in an entry's compared form.
func urlHostForm(h string) (string, error) {
	if a, err := netip.ParseAddr(h); err == nil {
		if a.Zone() != "" {
			return "", errors.New("zone")
		}
		return a.Unmap().String(), nil
	}
	return FoldHost(h), nil
}

// effectivePort is the URL's port, 443 when absent (the grant is https only).
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			return strconv.Itoa(n)
		}
		return p
	}
	return "443"
}

// AgentURLGuard is the guard for an agent project's approved API or MCP
// server (§6.7, §6.8). It is CustomerURLGuard(rawURL, nil), plus, when an
// entry grants the URL: PrivateHosts for the URL's host, which may then
// resolve to a private address (never loopback, link-local or a metadata
// address) on the URL's own port only, and the entry's pins, outside which
// no dial of the host is admitted (§6.12, GitHub #403).
func AgentURLGuard(rawURL string, entries []PrivateHost) DialGuard {
	g := CustomerURLGuard(rawURL, nil)
	e, host, ok := grantOf(rawURL, entries)
	if !ok {
		return g
	}
	// The dial sees the host as the transport spells it; every spelling of
	// this one host is granted.
	g.PrivateHosts = []string{host, FoldHost(host), strings.ToLower(host)}
	if a, err := netip.ParseAddr(host); err == nil {
		g.PrivateHosts = append(g.PrivateHosts, a.String(), a.Unmap().String())
	}
	u, _ := url.Parse(rawURL)
	g.PrivatePort = effectivePort(u)
	g.PrivatePins = append([]netip.Prefix(nil), e.Addresses...)
	return g
}

// AgentURLClient is CustomerURLClient with AgentURLGuard as its guard, read
// on every dial. When an entry grants the URL, the client follows only
// same-origin redirects, self-hosted too (§6.12 item 4): the grant is for one
// https origin. agentapi.Client refuses every redirect on top of it.
func AgentURLClient(rawURL string, entries []PrivateHost, timeout time.Duration) *http.Client {
	entries = append([]PrivateHost(nil), entries...)
	c := CustomerURLClient(rawURL, nil, timeout)
	c.Transport = liveClient(timeout, func() DialGuard { return AgentURLGuard(rawURL, entries) }).Transport
	if PrivateHostGranted(rawURL, entries) {
		c.CheckRedirect = SameHostRedirects
	}
	return c
}
