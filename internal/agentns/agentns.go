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
