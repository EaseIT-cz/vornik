package agentadmin

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"vornik.io/vornik/internal/agentns"
)

// AddAPIInput is add_api (design §8.3; plan P4.5).
type AddAPIInput struct {
	Project string       `json:"project"`
	Name    string       `json:"name"`
	BaseURL string       `json:"base_url"`
	Auth    APIAuthInput `json:"auth"`
	Methods []string     `json:"methods"`
	// Writes must be true exactly when Methods names a write; writes are
	// proposable only (plan P4.8).
	Writes bool `json:"writes,omitempty"`
}

// APIAuthInput names the header a credential is injected into.
type APIAuthInput struct {
	Header     string `json:"header,omitempty"`     // default Authorization
	Credential string `json:"credential,omitempty"` // the credential NAME; empty = none
	Prefix     string `json:"prefix,omitempty"`     // e.g. "Bearer "
}

// APIState is one project REST provider as rendered.
type APIState struct {
	Name, BaseURL, Header, AuthRef, Prefix string
	Methods                                []string
	Writes                                 bool
}

var (
	apiHeaderRe        = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	apiMethodOrder     = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}
	apiReservedHeaders = map[string]bool{"host": true, "content-length": true, "transfer-encoding": true,
		"connection": true, "cookie": true, "te": true, "upgrade": true}
)

func isReadMethod(m string) bool { return m == "GET" || m == "HEAD" }

// apiMethods validates and orders methods, split into read and write.
func apiMethods(in []string) (read, write []string, why string) {
	seen := map[string]bool{}
	for _, m := range in {
		if !contains(apiMethodOrder, m) {
			return nil, nil, fmt.Sprintf("method %q must be one of %s", m, strings.Join(apiMethodOrder, ", "))
		}
		seen[m] = true
	}
	for _, m := range apiMethodOrder {
		if !seen[m] {
			continue
		}
		if isReadMethod(m) {
			read = append(read, m)
		} else {
			write = append(write, m)
		}
	}
	if len(read)+len(write) == 0 {
		return nil, nil, "name at least one method"
	}
	return read, write, ""
}

func (r *Renderer) addAPI(st *State, raw json.RawMessage) (Change, error) {
	const verb = VerbAddAPI
	ns := st.Namespace
	var in AddAPIInput
	if err := decodeStrict(raw, &in); err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	pid := agentns.ID(ns, in.Project)
	p, ok := st.Projects[pid]
	if !ok {
		return refuse(verb, ns, "there is no project %q", pid), nil
	}
	if !nameRe.MatchString(in.Name) || strings.Contains(in.Name, "__") || strings.HasSuffix(in.Name, agentns.WriteSuffix) {
		return refuse(verb, ns, "API name %q must be a-z, 0-9, _ or - (not ending in %s)", in.Name, agentns.WriteSuffix), nil
	}
	if integrationTaken(p, in.Name) {
		return refuse(verb, ns, "%s already has a server or API named %q; remove it first", pid, in.Name), nil
	}
	api, why := apiStateFrom(ns, in)
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	read, write, _ := apiMethods(in.Methods)
	next := *p
	next.APIs = append(append([]APIState(nil), p.APIs...), api)
	if cred := in.Auth.Credential; cred != "" && !contains(p.Secrets, ns+"/"+cred) {
		next.Secrets = append(append([]string(nil), p.Secrets...), ns+"/"+cred)
	}
	yamlOut, err := r.renderProject(ns, &next)
	if err != nil {
		return Change{}, err
	}
	c := Change{
		Ops:     []FileOp{opFor(st, projectPath(pid), yamlOut)},
		ReadSet: map[string]string{},
		Locks:   []string{lockProject(pid), projectPath(pid), lockIntegration(pid, in.Name)},
		Class:   Widening,
		Grant:   Grant{Integrations: []IntegrationGrant{{Project: pid, Name: in.Name, Kind: "api", URL: api.BaseURL, Read: read, Write: write}}},
	}
	expect(st, c.ReadSet, projectPath(pid))
	c.Sentence = apiSentence(ns, pid, in, read, write)
	return c, nil
}

// apiStateFrom validates an add_api input into the rendered state.
func apiStateFrom(ns string, in AddAPIInput) (APIState, string) {
	if err := checkURL(in.BaseURL); err != nil {
		return APIState{}, err.Error()
	}
	if u, _ := url.Parse(in.BaseURL); u.RawQuery != "" {
		return APIState{}, "base_url may not carry a query"
	}
	read, write, why := apiMethods(in.Methods)
	if why != "" {
		return APIState{}, why
	}
	if in.Writes != (len(write) > 0) {
		return APIState{}, "writes must be true exactly when a write method (not GET or HEAD) is listed"
	}
	api := APIState{Name: in.Name, BaseURL: strings.TrimRight(in.BaseURL, "/"), Methods: append(append([]string(nil), read...), write...), Writes: in.Writes}
	a := in.Auth
	if a.Credential == "" {
		if a.Header != "" || a.Prefix != "" {
			return APIState{}, "auth needs a credential"
		}
		return api, ""
	}
	if !credentialRe.MatchString(a.Credential) {
		return APIState{}, fmt.Sprintf("credential %q must be A-Z, 0-9 or _ and start with a letter", a.Credential)
	}
	api.Header = a.Header
	if api.Header == "" {
		api.Header = "Authorization"
	}
	if !apiHeaderRe.MatchString(api.Header) || apiReservedHeaders[strings.ToLower(api.Header)] {
		return APIState{}, fmt.Sprintf("header %q is not a usable header name", api.Header)
	}
	if strings.ContainsAny(a.Prefix, "\r\n\x00") || len(a.Prefix) > 32 {
		return APIState{}, "prefix is not usable"
	}
	api.AuthRef, api.Prefix = "secret://"+ns+"/"+a.Credential, a.Prefix
	return api, ""
}

func apiSentence(ns, pid string, in AddAPIInput, read, write []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your assistant (%s) wants the project %q to call the API %q at %s", ns, pid, in.Name, hostOf(in.BaseURL))
	if len(read) > 0 {
		fmt.Fprintf(&b, " with %s", strings.Join(read, ", "))
	}
	if in.Auth.Credential != "" {
		fmt.Fprintf(&b, ", using the credential %s", in.Auth.Credential)
	}
	b.WriteString(".")
	if len(write) > 0 {
		fmt.Fprintf(&b, " It could also propose changes (%s). Each change will be shown to you for approval before it is made.", strings.Join(write, ", "))
	}
	return b.String()
}

// integrationTaken reports whether name is a server or API of the project:
// the approval table has one namespace for both.
func integrationTaken(p *ProjectState, name string) bool {
	for _, s := range p.Servers {
		if agentns.IntegrationOf(s.Name) == name {
			return true
		}
	}
	for _, a := range p.APIs {
		if a.Name == name {
			return true
		}
	}
	return false
}

// liveAPIs lists the project's APIs with a live approval, sorted.
func liveAPIs(st *State, p *ProjectState) []string {
	var out []string
	for _, a := range p.APIs {
		if ap, ok := st.Approvals[p.ID][a.Name]; ok && ap.Live() && ap.Kind == "api" {
			out = append(out, a.Name)
		}
	}
	sort.Strings(out)
	return out
}
