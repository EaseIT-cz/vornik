package service

import (
	"context"
	"errors"
	"net"
	"net/url"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/persistence"
)

// cgnatResolveTimeout bounds one host's resolution, and cgnatRunTimeout all
// of them, so a doctor run's host resolution waits at most 10 s however many
// rows there are (the table reads are bounded by the caller's context).
const (
	cgnatResolveTimeout = 2 * time.Second
	cgnatRunTimeout     = 10 * time.Second
)

// agentCGNATStatus is the source of the doctor finding agent_connections_cgnat
// (agent-administered design part 06 §6.11, GitHub #893): the live approved
// connections of every agent namespace with a loaded project, judged by the
// address the daemon's resolver returns now.
func (c *Container) agentCGNATStatus(ctx context.Context) (api.AgentCGNATStatus, error) {
	if c.Registry == nil || c.repos == nil || c.repos.AgentGrants == nil {
		return api.AgentCGNATStatus{}, errors.New("the registry or the approval tables are not available")
	}
	namespaces := c.loadedAgentNamespaces()
	// The run budget bounds resolution only: the table reads keep ctx, so a
	// slow resolver leaves hosts unresolved rather than failing the reads.
	rctx, cancel := context.WithTimeout(ctx, cgnatRunTimeout)
	defer cancel()
	resolve := func(_ context.Context, host string) ([]net.IP, error) { return resolveHostIPs(rctx, host) }
	return agentCGNATConnections(ctx, namespaces, c.repos.AgentGrants.ListIntegrations, c.agentPrivateAPIHosts(), resolve)
}

// resolveHostIPs resolves host with the daemon's resolver, bounded.
func resolveHostIPs(ctx context.Context, host string) ([]net.IP, error) {
	rctx, cancel := context.WithTimeout(ctx, cgnatResolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(rctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

// agentCGNATConnections examines each live (not removed) integration row of
// the namespaces. An address counts when it is in 100.64.0.0/10 and a
// private_api_hosts grant can admit it, so 100.100.100.200 (refused as a
// metadata address, listed or not) does not. A host that does not resolve is named, not silently skipped.
// Each distinct host is resolved once.
func agentCGNATConnections(ctx context.Context, namespaces []string,
	list func(context.Context, string) ([]persistence.AgentIntegrationApproval, error),
	privateHosts []dialguard.PrivateHost, resolve func(context.Context, string) ([]net.IP, error),
) (api.AgentCGNATStatus, error) {
	var st api.AgentCGNATStatus
	resolved := map[string][]net.IP{}
	for _, ns := range namespaces {
		rows, err := list(ctx, ns)
		if err != nil {
			return api.AgentCGNATStatus{}, err
		}
		for _, r := range rows {
			if r.RemovedAt != nil {
				continue
			}
			st.Examined++
			host := r.URL // named as written when it does not parse
			if u, err := url.Parse(r.URL); err == nil {
				host = u.Hostname()
			}
			ips := hostIPs(ctx, host, resolved, resolve)
			if len(ips) == 0 {
				st.Unresolved = append(st.Unresolved, r.Namespace+"/"+r.ProjectID+"/"+r.Integration+" ("+host+")")
				continue
			}
			st.Resolved++
			if addrs := cgnatAddrs(ips); len(addrs) > 0 {
				st.Found = append(st.Found, api.AgentCGNATConnection{
					Namespace: r.Namespace, ProjectID: r.ProjectID, Integration: r.Integration,
					Host: host, Addrs: addrs, Listed: dialguard.PrivateHostGranted(r.URL, dialguard.PrivateHostsFor(r.ProjectID, privateHosts)),
				})
			}
		}
	}
	return st, nil
}

// hostIPs is host's addresses: an IP literal is itself; a name is resolved
// once and remembered in cache. Nil when it does not resolve.
func hostIPs(ctx context.Context, host string, cache map[string][]net.IP,
	resolve func(context.Context, string) ([]net.IP, error),
) []net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}
	}
	if host == "" {
		return nil
	}
	ips, ok := cache[host]
	if !ok {
		ips, _ = resolve(ctx, host)
		cache[host] = ips
	}
	return ips
}

// cgnatAddrs is the addresses of ips in 100.64.0.0/10 other than a metadata
// address. Since #893 option A the guard refuses them unless the host is
// listed; a listed https host's grant admits them.
func cgnatAddrs(ips []net.IP) []string {
	var out []string
	for _, ip := range ips {
		if dialguard.IsSharedAddressSpace(ip) && !dialguard.IsCloudMetadata(ip) {
			out = append(out, ip.String())
		}
	}
	return out
}
