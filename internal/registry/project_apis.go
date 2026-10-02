package registry

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// ProjectAPI is a per-project REST provider (agent-administered Vornik
// design §8.3; plan P4.5). In this release only agent-namespace projects
// declare them; operator projects use gateway.providers.
type ProjectAPI struct {
	Name    string         `yaml:"name"`
	BaseURL string         `yaml:"base_url"`
	Auth    ProjectAPIAuth `yaml:"auth,omitempty"`
	// Methods are the HTTP methods the API is approved for.
	Methods []string `yaml:"methods"`
	// Writes is true exactly when Methods names a write (anything but GET
	// and HEAD). Writes are never called by a role: a workflow proposes
	// them, one approval each (plan P4.8).
	Writes bool `yaml:"writes,omitempty"`
}

// ProjectAPIAuth injects one header from a secret at call time.
type ProjectAPIAuth struct {
	Header    string `yaml:"header,omitempty"`
	ValueFrom string `yaml:"value_from,omitempty"`
	Prefix    string `yaml:"prefix,omitempty"`
}

// APIReadMethods are the methods a role may call (plan P4.5).
var APIReadMethods = map[string]bool{"GET": true, "HEAD": true}

var (
	apiNameRe   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	apiHeaderRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	apiMethods  = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
	// apiReservedHeaders are transport headers a credential must not set.
	apiReservedHeaders = map[string]bool{"host": true, "content-length": true, "transfer-encoding": true,
		"connection": true, "cookie": true, "te": true, "upgrade": true}
)

// HasWrites reports whether the API names a write method.
func (a ProjectAPI) HasWrites() bool {
	for _, m := range a.Methods {
		if !APIReadMethods[m] {
			return true
		}
	}
	return false
}

func validateAPIs(file string, p *Project) error {
	seen := map[string]bool{}
	for i, a := range p.APIs {
		field := fmt.Sprintf("apis[%d]", i)
		if err := validateAPI(a, p.Permissions.Secrets); err != nil {
			return ProjectValidationError{File: file, Field: field, Message: err.Error()}
		}
		if seen[a.Name] {
			return ProjectValidationError{File: file, Field: field, Message: fmt.Sprintf("the API %q is declared twice", a.Name)}
		}
		seen[a.Name] = true
	}
	return nil
}

func validateAPI(a ProjectAPI, granted []string) error {
	if !apiNameRe.MatchString(a.Name) || strings.HasSuffix(a.Name, "-write") {
		return fmt.Errorf("name %q must be a-z, 0-9, _ or - and not end in -write", a.Name)
	}
	if err := checkAPIBaseURL(a.BaseURL); err != nil {
		return err
	}
	if len(a.Methods) == 0 {
		return fmt.Errorf("API %q names no methods", a.Name)
	}
	for _, m := range a.Methods {
		if !apiMethods[m] {
			return fmt.Errorf("API %q: method %q must be one of GET, HEAD, POST, PUT, PATCH, DELETE", a.Name, m)
		}
	}
	if a.Writes != a.HasWrites() {
		return fmt.Errorf("API %q: writes must be true exactly when a write method (not GET or HEAD) is listed", a.Name)
	}
	return validateAPIAuth(a, granted)
}

func validateAPIAuth(a ProjectAPI, granted []string) error {
	au := a.Auth
	if au.ValueFrom == "" {
		if au.Header != "" || au.Prefix != "" {
			return fmt.Errorf("API %q: auth needs value_from", a.Name)
		}
		return nil
	}
	name, ok := strings.CutPrefix(au.ValueFrom, "secret://")
	if !ok || name == "" {
		return fmt.Errorf("API %q: value_from must be a secret:// reference", a.Name)
	}
	allowed := false
	for _, g := range granted {
		if g == name {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("API %q: the secret %q is not in permissions.secrets", a.Name, name)
	}
	if au.Header != "" && (!apiHeaderRe.MatchString(au.Header) || apiReservedHeaders[strings.ToLower(au.Header)]) {
		return fmt.Errorf("API %q: header %q is not a usable header name", a.Name, au.Header)
	}
	if strings.ContainsAny(au.Prefix, "\r\n\x00") || len(au.Prefix) > 32 {
		return fmt.Errorf("API %q: prefix is not usable", a.Name)
	}
	return nil
}

// checkAPIBaseURL accepts https anywhere and http only to a loopback host.
func checkAPIBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return fmt.Errorf("base_url %q must be a plain URL (no credentials, query or fragment)", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if ip := net.ParseIP(h); h == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("base_url %q must be https (or http on this machine)", raw)
}
