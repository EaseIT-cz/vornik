package agentadmin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"vornik.io/vornik/internal/agentns"
)

// Accounts (agent-administered design §22.2; GitHub #119 item 8). An
// account is a named connection instance: each agent connection (an MCP
// server or a REST API) acts as one account, named by a label and,
// optionally, an identity. The label lives in the renderer-owned project
// file (so the reach signature can bind it) and in the approval row; the
// identity lives only in the approved document and the approval row.

// AccountInput is the optional account of add_mcp_server and add_api.
type AccountInput struct {
	Label    string `json:"label"`
	Identity string `json:"identity,omitempty"`
}

// AccountGrant is the account an approval names: in IntegrationGrant (the
// approved document), and later in a write action's request. Omitted when
// the label is the default and there is no identity, so the document of
// every default-account connection is unchanged.
type AccountGrant struct {
	Label          string `json:"label"`
	Identity       string `json:"identity,omitempty"`
	IdentitySource string `json:"identity_source,omitempty"`
}

// Identity sources (design §22.2.1).
const (
	IdentityFromAgent    = "agent"
	IdentityFromProvider = "provider"
)

// AccountRule is the agent-facing statement of what an account is; the tool
// schemas render it so the text and the code cannot drift.
const AccountRule = "Optional: which of the user's accounts this connection acts as. label: a-z, 0-9 and dashes, starting with a letter, " +
	"at most 32 (default \"default\"); identity: optionally, the account's address or name as the user told you (letters, digits, " +
	"spaces and . _ @ + -, at most 128). A project may connect to one host twice only with different labels, and a credential name " +
	"belongs to one account. When the user has two accounts at one provider, ask which; never pick one."

// identityRe admits an address or a plain name: no brackets, quotes,
// semicolons or other punctuation, so an identity cannot close the phrase
// it sits in and append words to an approval sentence (design §22.2.1).
var identityRe = regexp.MustCompile(`^[A-Za-z0-9 ._@+-]{1,128}$`)

// checkAccount validates an add verb's account and returns the label it
// renders ("" for the default) and the grant's account (nil for the default
// with no identity). The values are never echoed in a refusal.
func checkAccount(in *AccountInput) (label string, grant *AccountGrant, why string) {
	if in == nil {
		return "", nil, ""
	}
	if !agentns.ValidAccountLabel(in.Label) {
		return "", nil, "the account label must be a-z, 0-9 or dashes, start with a letter and be at most 32 characters"
	}
	if in.Identity != "" && (!identityRe.MatchString(in.Identity) || strings.TrimSpace(in.Identity) != in.Identity || strings.Contains(in.Identity, "  ")) {
		return "", nil, "the account identity may hold only letters, digits, single spaces and . _ @ + - (at most 128, no leading or trailing space)"
	}
	if in.Label != agentns.DefaultAccount {
		label = in.Label
	}
	if label == "" && in.Identity == "" {
		return "", nil, ""
	}
	g := &AccountGrant{Label: agentns.AccountOf(label), Identity: in.Identity}
	if in.Identity != "" {
		g.IdentitySource = IdentityFromAgent
	}
	return label, g, ""
}

// hostKey is the host a connection reaches, for the two-connections rule:
// the URL's host and port, lower-cased.
func hostKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(raw)
	}
	return strings.ToLower(u.Host)
}

// connection is one agent connection of a project, for the account rules.
type connection struct {
	project, name, url, authRef, label string
	oauth                              bool
}

// connectionsOf lists a project's connections (MCP base entries and APIs);
// a write sibling is the same connection as its base.
func connectionsOf(p *ProjectState) []connection {
	var out []connection
	for _, s := range p.Servers {
		if strings.HasSuffix(s.Name, agentns.WriteSuffix) {
			continue
		}
		out = append(out, connection{project: p.ID, name: s.Name, url: s.URL, authRef: s.AuthRef, label: agentns.AccountOf(s.Account), oauth: s.OAuth})
	}
	for _, a := range p.APIs {
		out = append(out, connection{project: p.ID, name: a.Name, url: a.BaseURL, authRef: a.AuthRef, label: agentns.AccountOf(a.Account)})
	}
	return out
}

// credentialKey is what makes a connection an account: its static
// credential, or its own OAuth sign-in; "" for a connection with no
// credential, which acts as no account.
func (c connection) credentialKey() string {
	if c.oauth {
		return "oauth:" + c.name
	}
	return c.authRef
}

// sameHostSameLabel is the two-connections rule (design §22.2.1, as built:
// §22.2.15 B3): a project may connect to one host with two credentials only
// under distinct labels. Two connections that share one credential are one
// account (rule C1) and may share its label; a connection with no
// credential acts as no account and is not judged. It returns the refusal,
// or "".
func sameHostSameLabel(p *ProjectState, add connection) string {
	return sameHostSameLabelWithRemedy(p, add, "give this one another account label")
}

// sameHostSameLabelWithRemedy shares the connection/account predicate while
// letting verbs name their own input as the recovery path. Recipe installs
// must direct the agent to install_recipe.account; single-connection verbs
// retain their existing guidance.
func sameHostSameLabelWithRemedy(p *ProjectState, add connection, remedy string) string {
	key := add.credentialKey()
	if key == "" {
		return ""
	}
	host, want := hostKey(add.url), add.label
	for _, c := range connectionsOf(p) {
		if hostKey(c.url) == host && c.label == want && c.credentialKey() != "" && c.credentialKey() != key {
			return fmt.Sprintf("this project already connects to %s as your %s account (%s); %s", host, want, c.name, remedy)
		}
	}
	return ""
}

// reachAccounts maps each integration a workflow reaches to its account
// phrase (design §22.2.5); nil when none has one to say.
func reachAccounts(st *State, p *ProjectState, sig ReachSignature) map[string]string {
	var out map[string]string
	for _, n := range sig.Integrations {
		if phrase := connectionPhrase(st, p, strings.TrimPrefix(n, "api:")); phrase != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[n] = phrase
		}
	}
	return out
}

// Keys of Change.accounts that are not integration names.
const (
	slotAccountKey  = "\x00slot"
	slotRotationKey = "\x00rotation"
)

// accountWords is §22.2.5's phrase: "" for a default account with no
// identity that is alone at its host (so every existing sentence reads as
// before), "your account (<identity>)" for a default one with an identity,
// otherwise "your <label> account (<identity or not verified>)". An
// agent-described identity is marked as such.
func accountWords(label, identity, source string, named bool) string {
	label = agentns.AccountOf(label)
	id := "not verified"
	switch {
	case identity != "" && source == IdentityFromProvider:
		id = identity
	case identity != "":
		id = identity + " as your assistant described it; not verified"
	}
	if label == agentns.DefaultAccount && !named {
		if identity == "" {
			return ""
		}
		return "your account (" + id + ")"
	}
	return "your " + label + " account (" + id + ")"
}

// asAccount is " as <phrase>", or "" for no phrase.
func asAccount(phrase string) string {
	if phrase == "" {
		return ""
	}
	return " as " + phrase
}

// sharesHost reports whether another connection of p than name reaches
// url's host: then even the default account is named (§22.2.5).
func sharesHost(p *ProjectState, name, rawURL string) bool {
	host := hostKey(rawURL)
	for _, c := range connectionsOf(p) {
		if c.name != name && hostKey(c.url) == host {
			return true
		}
	}
	return false
}

// connectionPhrase is the account phrase of an existing connection of p:
// its label from the project file, its identity from the approval row.
func connectionPhrase(st *State, p *ProjectState, name string) string {
	if p == nil {
		return ""
	}
	for _, c := range connectionsOf(p) {
		if c.name != name {
			continue
		}
		a := st.Approvals[p.ID][name]
		return accountWords(c.label, a.AccountIdentity, a.IdentitySource, c.label != agentns.DefaultAccount || sharesHost(p, name, c.url))
	}
	return ""
}

// ConnectionPhrase is connectionPhrase for the service (a write action's
// sentence, design §22.2.6).
func ConnectionPhrase(st *State, p *ProjectState, name string) string {
	return connectionPhrase(st, p, name)
}

// ConnectionLabel is the account label of p's connection name as its
// project file declares it ("default" when none), and whether p has it.
func ConnectionLabel(p *ProjectState, name string) (string, bool) {
	for _, c := range connectionsOf(p) {
		if c.name == name {
			return c.label, true
		}
	}
	return "", false
}

// addPhrase is the account phrase of a connection being added.
func addPhrase(p *ProjectState, name, rawURL string, g *AccountGrant) string {
	label, identity, source := "", "", ""
	if g != nil {
		label, identity, source = g.Label, g.Identity, g.IdentitySource
	}
	return accountWords(label, identity, source, agentns.AccountOf(label) != agentns.DefaultAccount || sharesHost(p, name, rawURL))
}

// credentialUsersInNamespace is C5's list: the requesting project's users
// as "<name> at <host>" (today's wording), then every other project's as
// "<name> in <project> at <host>".
func credentialUsersInNamespace(st *State, p *ProjectState, ns, name string) []string {
	out := credentialUsers(p, ns, name)
	ref := "secret://" + ns + "/" + name
	for _, q := range sortedProjects(st) {
		if q.ID == p.ID {
			continue
		}
		var more []string
		for _, c := range connectionsOf(q) {
			if c.authRef == ref {
				more = append(more, c.name+" in "+q.ID+" at "+hostOf(c.url))
			}
		}
		sort.Strings(more)
		out = append(out, more...)
	}
	return out
}

// credentialAccount is the account a credential slot names: the phrase and
// the grant of the connections that use it (one account by C1), from the
// requesting project's first user; "" and nil for the default account with
// no identity alone at its host.
func credentialAccount(st *State, p *ProjectState, ns string, in RequestCredentialInput) (string, *AccountGrant) {
	ref := "secret://" + ns + "/" + in.Name
	for _, c := range connectionsOf(p) {
		if (in.Kind == CredentialSecret && c.authRef == ref) || (in.Kind == CredentialOAuth && c.oauth && OAuthCredentialName(c.name) == in.Name) {
			phrase := connectionPhrase(st, p, c.name)
			if phrase == "" {
				return "", nil
			}
			a := st.Approvals[p.ID][c.name]
			return phrase, &AccountGrant{Label: c.label, Identity: a.AccountIdentity, IdentitySource: a.IdentitySource}
		}
	}
	return "", nil
}

// providerEmailRe is the only shape a provider identity may take (design
// §22.2.7): no brackets, quotes, semicolons or spaces, so a server that
// names its own token endpoint cannot close the phrase it sits in.
var providerEmailRe = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,190}$`)

// EmailFromIDToken returns the verified email an OAuth token response's
// id_token carries (design §22.2.7): the payload is decoded without a
// signature check (it arrived over TLS directly from the token endpoint,
// OIDC Core 3.1.3.7), the email is taken only when email_verified is the
// boolean true, and only in providerEmailRe's shape. Anything else is "".
// The identity is a label for the person; nothing authorizes on it.
func EmailFromIDToken(raw string) (string, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || len(raw) > 16*1024 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", false
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", false
	}
	if v, ok := claims.EmailVerified.(bool); !ok || !v || !providerEmailRe.MatchString(claims.Email) {
		return "", false
	}
	return claims.Email, true
}

// staticRef is the secret reference a static MCP auth block binds, or "".
func staticRef(ns string, a MCPAuthInput) string {
	if a.Mode != "static" || !credentialRe.MatchString(a.Credential) {
		return ""
	}
	return "secret://" + ns + "/" + a.Credential
}

// credentialOwner is rule C1 (design §22.2.3): a credential NAME belongs to
// one account label across the namespace. It returns the refusal when a
// connection of another label names authRef, and otherwise the connections
// that already share it (for C4's sentence), in project and name order.
func credentialOwner(st *State, authRef, label string) (why string, sharers []connection) {
	if authRef == "" {
		return "", nil
	}
	want := agentns.AccountOf(label)
	for _, p := range sortedProjects(st) {
		for _, c := range connectionsOf(p) {
			if c.authRef != authRef {
				continue
			}
			if c.label != want {
				name := strings.TrimPrefix(authRef, "secret://"+st.Namespace+"/")
				return fmt.Sprintf("the credential %s belongs to your %s account (%s in %s); use another credential name for this account", name, c.label, c.name, c.project), nil
			}
			sharers = append(sharers, c)
		}
	}
	return "", sharers
}

// sharedCredentialSentence is rule C4's clause: "" for a first use.
func sharedCredentialSentence(credential string, sharers []connection) string {
	if len(sharers) == 0 {
		return ""
	}
	var with []string
	for _, c := range sharers {
		with = append(with, c.name+" in "+c.project)
	}
	return fmt.Sprintf(" It shares the credential %s with %s: both act as the same account.", credential, strings.Join(with, " and "))
}

// sortedProjects lists the namespace's projects by ID (a range, never a
// lookup by input: the §5 T7 read guard).
func sortedProjects(st *State) []*ProjectState {
	out := make([]*ProjectState, 0, len(st.Projects))
	for _, p := range st.Projects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// oauthSlotTaken is rule C2 (design §22.2.3): two OAuth servers of a
// project never share one sign-in slot. It returns the refusal, or "".
func oauthSlotTaken(p *ProjectState, name string) string {
	slot := OAuthCredentialName(name)
	for _, c := range connectionsOf(p) {
		if c.oauth && c.name != name && OAuthCredentialName(c.name) == slot {
			return fmt.Sprintf("the server name %q would share the sign-in %s with %q; choose another name", name, slot, c.name)
		}
	}
	return ""
}
