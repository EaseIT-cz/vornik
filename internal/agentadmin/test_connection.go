package agentadmin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentns"
)

// test_connection (design §6.2, GitHub #101): probe one approved connection
// with its stored credential and answer with one closed code. The verb is a
// read: it files nothing and needs no approval.

// VerbTestConnection is the verb's name.
const VerbTestConnection = "test_connection"

// ConnectionCode is the closed result of a connection probe. It is exported
// for the agentic log tools (#92), which report connection failures with the
// same codes.
type ConnectionCode string

// The codes, in the order the probe decides them (design §6.2 table).
const (
	ConnOK                ConnectionCode = "ok"
	ConnAuthRejected      ConnectionCode = "auth_rejected"
	ConnUnreachable       ConnectionCode = "unreachable"
	ConnTLSError          ConnectionCode = "tls_error"
	ConnNotApproved       ConnectionCode = "not_approved"
	ConnCredentialMissing ConnectionCode = "credential_missing"
	ConnUnknown           ConnectionCode = "unknown"
)

// ConnectionResult is the whole answer: no body, header, URL, tool name or
// upstream error text ever rides along.
type ConnectionResult struct {
	Code            ConnectionCode `json:"code"`
	HTTPStatusClass string         `json:"http_status_class"`
	CheckedAt       time.Time      `json:"checked_at"`
	// Account is the connection's account label and IdentitySource where
	// its identity came from (agent-administered design §22.2.9; GitHub #119
	// item 8). Both are omitted for a default account with no identity, so
	// that answer keeps exactly the three keys. A provider-reported identity
	// itself is never returned.
	Account        string `json:"account,omitempty"`
	IdentitySource string `json:"identity_source,omitempty"`
}

// TestConnectionRule is the agent-facing statement of what the answer means;
// the tool description renders it.
const TestConnectionRule = "Returns {code, http_status_class, checked_at}, and for a connection with a named account also account (its label) and identity_source (agent or provider). code is ok, auth_rejected, unreachable, tls_error, not_approved, credential_missing or unknown. " +
	"ok means the provider answered and did not refuse the credential; ok with 4xx means it answered and did not refuse the credential, not that the request was served. " +
	"A stored credential is not proof the provider accepts it: call this after the user enters one, before you define a workflow that uses it."

// TestConnectionInput is test_connection's input.
type TestConnectionInput struct {
	Project string `json:"project"`
	Name    string `json:"name"`
}

// ParseTestConnectionInput decodes and checks the input for namespace ns.
// The project is normalised by ProjectRefRule (§5, T7); the namespace always
// comes from the key, never from the input.
func ParseTestConnectionInput(ns string, raw json.RawMessage) (TestConnectionInput, error) {
	var in TestConnectionInput
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	in.Project = projectSlug(ns, in.Project)
	if err := checkSlug("project", in.Project); err != nil {
		return in, err
	}
	if !nameRe.MatchString(in.Name) || strings.HasSuffix(in.Name, agentns.WriteSuffix) {
		return in, fmt.Errorf("name %q must be the name of a server or API of the project, as list_my_setup shows it", in.Name)
	}
	return in, nil
}

// StatusClass is "1xx".."5xx" for an HTTP status, "" when there was none.
func StatusClass(status int) string {
	if status < 100 || status > 599 {
		return ""
	}
	return fmt.Sprintf("%dxx", status/100)
}

// ClassifyConnectionError maps a probe's outcome to a code and the status
// class. err is the transport error (nil when a response arrived); status is
// the HTTP status, 0 when none. It reads only typed errors and the status,
// never a message (design §6.2).
func ClassifyConnectionError(err error, status int) (ConnectionCode, string) {
	class := StatusClass(status)
	if status == 401 || status == 403 {
		return ConnAuthRejected, class
	}
	if err == nil {
		if status == 429 || status >= 500 || status == 0 {
			return ConnUnknown, class
		}
		return ConnOK, class
	}
	if status != 0 {
		return ConnUnknown, class
	}
	if isTLSError(err) {
		return ConnTLSError, ""
	}
	var opErr *net.OpError
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.As(err, &opErr), errors.As(err, &dnsErr), errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &netErr) && netErr.Timeout():
		return ConnUnreachable, ""
	}
	return ConnUnknown, ""
}

func isTLSError(err error) bool {
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	return errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &host) ||
		errors.As(err, &invalid) || errors.As(err, &record)
}
