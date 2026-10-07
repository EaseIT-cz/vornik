package agentadmin

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"vornik.io/vornik/internal/agentns"
)

// RequestCredentialInput is request_credential (design §8.2). There is no
// value field: a value is entered only on the approver device's page.
type RequestCredentialInput struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	Kind    string `json:"kind"` // secret | oauth
}

// Credential kinds.
const (
	CredentialSecret = "secret"
	CredentialOAuth  = "oauth"
)

// CredentialSlot is a credential_slot request's typed content: which
// credential the device page asks for. Entering the value is the approval.
type CredentialSlot struct {
	Namespace string `json:"namespace"`
	Project   string `json:"project"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	// Server is the OAuth server a kind=oauth slot connects.
	Server string `json:"server,omitempty"`
	// UsedBy names each server (or API) that references the credential, as
	// "<name> at <host>".
	UsedBy []string `json:"used_by"`
}

func lockCredential(ns, name string) string { return "credential:" + ns + "/" + name }

// requestCredential renders a credential slot. It writes no file: the server
// or API that uses the credential was approved when it was added, and the
// value goes to the secret store from the device page.
func (r *Renderer) requestCredential(st *State, raw json.RawMessage) Change {
	const verb = VerbRequestCredential
	ns := st.Namespace
	var in RequestCredentialInput
	if err := decodeStrict(raw, &in); err != nil {
		return refuse(verb, ns, "%v", err)
	}
	in.Project = projectSlug(ns, in.Project)
	pid := agentns.ID(ns, in.Project)
	p, ok := st.Projects[pid]
	if !ok {
		return refuse(verb, ns, "there is no project %q", pid)
	}
	if !credentialRe.MatchString(in.Name) || !agentns.ValidSecretName(in.Name) {
		return refuse(verb, ns, "credential %q must be A-Z, 0-9 or _ and start with a letter", in.Name)
	}
	if err := checkOneLine("purpose", in.Purpose, true); err != nil {
		return refuse(verb, ns, "%v", err)
	}
	var usedBy []string
	server := ""
	switch in.Kind {
	case CredentialSecret:
		usedBy = credentialUsers(p, ns, in.Name)
	case CredentialOAuth:
		for _, s := range p.Servers {
			if s.OAuth && !strings.HasSuffix(s.Name, agentns.WriteSuffix) && OAuthCredentialName(s.Name) == in.Name {
				// One server per token name: OAuthCredentialName is
				// injective over server names (review 20261002-5d00 F3).
				server = s.Name
				usedBy = []string{s.Name + " at " + hostOf(s.URL)}
				break
			}
		}
	default:
		return refuse(verb, ns, "kind %q must be secret or oauth", in.Kind)
	}
	if len(usedBy) == 0 {
		return refuse(verb, ns, "no server or API of %s uses the %s credential %s; add the server first", pid, in.Kind, in.Name)
	}
	slot := &CredentialSlot{Namespace: ns, Project: pid, Name: in.Name, Kind: in.Kind, Server: server, UsedBy: usedBy}
	how := "Enter it on this page; your assistant will never see it."
	if in.Kind == CredentialOAuth {
		how = "Tap Connect on this page to sign in there; your assistant will never see the sign-in."
	}
	c := Change{
		Class: Widening,
		Slot:  slot,
		Locks: []string{lockCredential(ns, in.Name)},
		Sentence: "Your assistant (" + ns + ") asks you to add the credential " + in.Name + " for " + pid +
			", used by " + strings.Join(usedBy, ", ") + ", for: " + quoteShort(in.Purpose) +
			". " + how,
	}
	return c
}

// credentialUsers lists the project's servers that reference ns/name.
func credentialUsers(p *ProjectState, ns, name string) []string {
	ref := "secret://" + ns + "/" + name
	var out []string
	for _, s := range p.Servers {
		if s.AuthRef == ref && !strings.HasSuffix(s.Name, agentns.WriteSuffix) {
			out = append(out, s.Name+" at "+hostOf(s.URL))
		}
	}
	for _, a := range p.APIs {
		if a.AuthRef == ref {
			out = append(out, "the API "+a.Name+" at "+hostOf(a.BaseURL))
		}
	}
	sort.Strings(out)
	return out
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// OAuthCredentialName is the token slot of an OAuth server: OAUTH_ and the
// server name upper-cased, dashes as underscores.
func OAuthCredentialName(server string) string {
	return "OAUTH_" + strings.ToUpper(strings.ReplaceAll(server, "-", "_"))
}
