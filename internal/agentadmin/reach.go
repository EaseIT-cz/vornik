package agentadmin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/registry"
)

// ReachSignature is what a workflow approval binds (§7.6; plan amendment 4):
// what returns (Egress), where it can reach (Integrations), with which
// credentials (Credentials), what it may write (Proposes) and when it runs
// unattended (Schedule). Step order, conditionals, retries, instructions and
// the input schema are deliberately NOT in it: none of them adds a
// destination or a credential.
type ReachSignature struct {
	Egress       json.RawMessage `json:"egress"`
	Integrations []string        `json:"integrations"`
	Credentials  []string        `json:"credentials"`
	Proposes     json.RawMessage `json:"proposes"`
	Schedule     string          `json:"schedule"`
}

// SignatureOf computes a workflow's reach from LOADED config objects. The
// renderer calls it over its own output re-parsed by the registry's parsers,
// and the executor over the live registry, so both sides run the same code
// over the same kind of object.
func SignatureOf(p *registry.Project, sw *registry.Swarm, wf *registry.Workflow) (ReachSignature, error) {
	var sig ReachSignature
	if wf == nil || wf.Broker == nil {
		return sig, fmt.Errorf("workflow is not a broker workflow")
	}
	egress, err := canonicalOf(wf.Broker.Egress)
	if err != nil {
		return sig, fmt.Errorf("egress: %w", err)
	}
	sig.Egress = egress
	proposes, err := canonicalOf(wf.Broker.Proposes)
	if err != nil {
		return sig, fmt.Errorf("proposes: %w", err)
	}
	sig.Proposes = proposes
	sched, err := scheduleSignature(wf.Broker.Schedule)
	if err != nil {
		return sig, fmt.Errorf("schedule: %w", err)
	}
	sig.Schedule = sched

	// The roles the steps run, and every integration their tools reach.
	roles := map[string]bool{}
	for _, st := range wf.Steps {
		if st.Role != "" {
			roles[st.Role] = true
		}
	}
	servers := map[string]bool{}
	if sw != nil {
		for _, r := range sw.Roles {
			if !roles[r.Name] {
				continue
			}
			for _, t := range r.Permissions.AllowedTools {
				if srv, _, ok := splitMCPTool(t); ok {
					servers[srv] = true
				}
				if t == QueryAPITool {
					addAPIReach(p, servers)
				}
			}
		}
	}
	creds := credentialsOf(p, servers)
	sig.Integrations = sortedKeys(servers)
	sig.Credentials = sortedKeys(creds)
	return sig, nil
}

// Hash is the sha256 of the signature's canonical JSON.
func (s ReachSignature) Hash() string {
	raw, _ := json.Marshal(s)
	canon, err := approval.Canonical(raw)
	if err != nil {
		canon = raw
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// splitMCPTool splits "mcp__<server>__<tool>".
func splitMCPTool(name string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, "mcp__")
	if !found {
		return "", "", false
	}
	server, tool, found = strings.Cut(rest, "__")
	if !found || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

func canonicalOf(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return approval.Canonical(raw)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// credentialsOf lists the credential handles of the servers a workflow's
// roles use: a static reference, or an OAuth grant with its scopes (plan
// P4.4: the scopes are the grant's reach).
func credentialsOf(p *registry.Project, servers map[string]bool) map[string]bool {
	creds := map[string]bool{}
	if p == nil {
		return creds
	}
	for _, a := range p.APIs {
		if ref := strings.TrimSpace(a.Auth.ValueFrom); ref != "" && servers["api:"+a.Name] {
			creds["api:"+a.Name+"="+ref] = true
		}
	}
	for _, s := range p.MCP.Servers {
		if !servers[s.Name] {
			continue
		}
		if ref := strings.TrimSpace(s.Auth.ValueFrom); ref != "" {
			creds[s.Name+"="+ref] = true
		}
		if s.Auth.Mode == "oauth" {
			sc := append([]string(nil), s.Auth.Scopes...)
			sort.Strings(sc)
			creds[s.Name+"=oauth:"+strings.Join(sc, " ")] = true
		}
	}
	return creds
}

// addAPIReach records every API of the project: a role holding query_api can
// call any of them (plan P4.5), so all of them are the workflow's reach.
func addAPIReach(p *registry.Project, servers map[string]bool) {
	if p == nil {
		return
	}
	for _, a := range p.APIs {
		servers["api:"+a.Name] = true
	}
}
