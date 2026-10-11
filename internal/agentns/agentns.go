// Package agentns is the single implementation of the agent-namespace rule
// (agent-administered Vornik design §5). Every ID an agent creates is
// <namespace>--<slug>; the registry, secret resolution, projectdoctor and
// agentadmin all ask this package, so the rule cannot drift between them.
package agentns

import (
	"regexp"
	"strings"
)

// Separator joins a namespace and a slug. No operator ID may contain it.
const Separator = "--"

var nsRe = regexp.MustCompile(`^[a-z][a-z0-9]{1,15}$`)

// Valid reports whether ns is a well-formed namespace slug.
func Valid(ns string) bool { return nsRe.MatchString(ns) }

// FromID returns the namespace of an agent-owned ID: the part before the
// first separator, when that part is a valid namespace and a non-empty slug
// follows. ok is false for every operator ID.
func FromID(id string) (string, bool) {
	ns, slug, found := strings.Cut(id, Separator)
	if !found || slug == "" || !Valid(ns) {
		return "", false
	}
	return ns, true
}

var secretNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// ValidSecretName reports whether name is a well-formed agent secret NAME,
// the part after "<ns>/" in secret://<ns>/<NAME>. The reference parser and
// the store both ask here, so they cannot disagree on what a NAME is.
func ValidSecretName(name string) bool { return secretNameRe.MatchString(name) }

// DefaultAccount is the label of a connection added without one
// (agent-administered design §22.2.1; GitHub #119 item 8).
const DefaultAccount = "default"

var accountLabelRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidAccountLabel reports whether label is a well-formed account label.
// The renderer and the registry's load rule both ask here.
func ValidAccountLabel(label string) bool { return accountLabelRe.MatchString(label) }

// AccountOf is a connection's account label: the default for "".
func AccountOf(label string) string {
	if label == "" {
		return DefaultAccount
	}
	return label
}

// OAuthSlot is the credential slot of an OAuth server: OAUTH_ and the
// server name upper-cased, dashes as underscores. It is not injective
// ("a-b" and "a_b" share a slot), so the renderer and the load rule refuse
// two OAuth servers of a project with one slot (agent-administered design
// §22.2.3 C2).
func OAuthSlot(server string) string {
	return "OAUTH_" + strings.ToUpper(strings.ReplaceAll(server, "-", "_"))
}

// ID builds an agent-owned ID.
func ID(ns, slug string) string { return ns + Separator + slug }

// WriteSuffix names an agent server's write entry: "<name>-write" is the
// broker_write sibling of the broker_read_only "<name>" (plan P4.3b), one
// integration with two entries.
const WriteSuffix = "-write"

// IntegrationOf maps an agent server entry to its integration: the write
// sibling to its base, any other name to itself. Everything that names an
// integration (the approval row, the gates, credential and token lookup)
// uses it.
func IntegrationOf(server string) string {
	return strings.TrimSuffix(server, WriteSuffix)
}
