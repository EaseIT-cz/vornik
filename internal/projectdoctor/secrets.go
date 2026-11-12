package projectdoctor

import (
	"context"
	"errors"

	"vornik.io/vornik/internal/persistence"
)

// CredentialWriter stores one operator credential and writes its
// admin_audit row (credential.set, the name, never the value). The service's
// operatorCredentials is the one implementation: the console page, the MCP
// form, the API and project setup share it.
type CredentialWriter interface {
	Put(ctx context.Context, name string, value []byte, who persistence.AdminAuditEntry) error
}

// StoreSecrets keeps a project's secrets in the encrypted store (GitHub #625
// item 3, #626; agent-administered Vornik design, amendment 2026-11-12 E).
// Before, Set wrote secrets/project-secrets.env in plaintext and called
// os.Setenv; now nothing reaches a file or the process environment.
//
// A stored value takes effect at its readers' next lookup: the point-of-use
// reads go through secretstore.Value, which consults the store. A
// named_secrets placeholder resolves at load, so it applies at the next
// config reload, as an os.Setenv value did.
type StoreSecrets struct {
	// Writer stores the value. Nil refuses every Set: there is no plaintext
	// fallback.
	Writer CredentialWriter
	// Lookup answers Has: the environment, then the store (the daemon's
	// secret source). Nil reports every name absent.
	Lookup interface {
		Get(name string) (string, bool)
	}
	// Who is the audit identity of the writes; an empty Source or Principal
	// defaults to "project-setup".
	Who persistence.AdminAuditEntry
}

// errNoCredentialStore is Set's refusal when no store is wired.
var errNoCredentialStore = errors.New("the credential store is not available; the secret was not saved")

// Has reports whether the environment or the store holds a non-empty value.
func (s StoreSecrets) Has(name string) bool {
	if s.Lookup == nil || name == "" {
		return false
	}
	v, ok := s.Lookup.Get(name)
	return ok && v != ""
}

// Set stores the secret as an operator credential and audits it.
func (s StoreSecrets) Set(name, value string) error {
	if s.Writer == nil {
		return errNoCredentialStore
	}
	who := s.Who
	if who.Source == "" {
		who.Source = "project-setup"
	}
	if who.Principal == "" {
		// An audit row always names who wrote (review 1dd3 F2).
		who.Principal = "project-setup"
	}
	return s.Writer.Put(context.Background(), name, []byte(value), who)
}
