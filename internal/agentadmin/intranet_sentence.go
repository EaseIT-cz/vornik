package agentadmin

import (
	"fmt"
	"net/url"
	"strings"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/integrations/dialguard"
)

// intranetSentence names, once each, the hosts of urls that an
// agent_admin.private_api_hosts entry grants, so the approver sees that the
// connection may reach their private network (agent-administered design
// part 06 §6.10, GitHub #403), and, when the entry pins addresses, where
// (§6.12). entries are already scoped to the project
// (dialguard.PrivateHostsFor). It decides with dialguard.PrivateHostGrant,
// the predicate the dial guard and the pre-approval listing mask use, and
// returns "" when no URL is granted.
func intranetSentence(entries []dialguard.PrivateHost, urls ...string) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, raw := range urls {
		e, ok := dialguard.PrivateHostGrant(raw, entries)
		if !ok {
			continue
		}
		host := hostOf(raw)
		if seen[dialguard.FoldHost(host)] {
			continue
		}
		seen[dialguard.FoldHost(host)] = true
		fmt.Fprintf(&b, " %s is on your list of intranet hosts (agent_admin.private_api_hosts)", host)
		if len(e.Addresses) > 0 {
			fmt.Fprintf(&b, ", only at %s", e.Pins())
		}
		b.WriteString(": this connection may reach an address on your private network, never this machine.")
	}
	return b.String()
}

// privateReach is IntegrationGrant.PrivateReach for a new destination: the
// text of the entry that grants rawURL, "" when none does (§6.12 item 5).
func privateReach(entries []dialguard.PrivateHost, rawURL string) string {
	if e, ok := dialguard.PrivateHostGrant(rawURL, entries); ok {
		return e.String()
	}
	return ""
}

// PrivateReachMoved is the apply check of §6.12 item 7: for each grant that
// names a new destination (no live integration of its project has its name
// and URL), the entry that grants it now, under the live list in st, must be
// the one the approval recorded. It returns why the apply must not proceed,
// or "". It reads the live list in st at apply time, however the list
// moved (today only a restart changes it). The reason names the URL, never
// the entry's pins or projects, which describe_installation also withholds
// (code review 6b2e F2).
func PrivateReachMoved(st *State, grants []IntegrationGrant) string {
	for _, g := range grants {
		if liveIntegration(st, g) {
			continue
		}
		now := privateReach(dialguard.PrivateHostsFor(g.Project, st.PrivateHosts), g.URL)
		if now == g.PrivateReach {
			continue
		}
		return fmt.Sprintf("the operator's list of intranet hosts changed after you were asked (what it grants %s is not what was approved); your assistant must ask again",
			redactedURL(g.URL))
	}
	return ""
}

// redactedURL is a URL's scheme, host and path, never its query or userinfo.
func redactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return hostOf(raw)
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// liveIntegration reports whether g names an integration its project already
// has, with the same URL: a tools approval or a recipe's tools change, not a
// new destination. Comparing the URL keeps the skip from resting on the
// invariant that no verb moves a live integration (review b6d3 F2).
func liveIntegration(st *State, g IntegrationGrant) bool {
	if ns, _ := agentns.FromID(g.Project); ns != st.Namespace {
		return false // not this namespace's: checked as a new destination
	}
	p, ok := st.Projects[g.Project]
	if !ok {
		return false
	}
	for _, s := range p.Servers {
		if s.Name == g.Name && s.URL == g.URL {
			return true
		}
	}
	for _, a := range p.APIs {
		if a.Name == g.Name && a.BaseURL == g.URL {
			return true
		}
	}
	return false
}
