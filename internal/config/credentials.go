package config

import (
	"flag"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// Configuration placeholders that resolve from the credential store (GitHub
// #626; agent-administered Vornik design, amendment 2026-11-12 A and B).
//
// A ${NAME} resolves from the environment when it holds a non-empty value,
// else from the operator credential of that name the caller looked up in
// the store and passed with WithCredentials, else to "". The store is a
// database table, and the database is opened with the configuration this
// load produces, so the daemon loads twice: LoadBootstrap validates only the
// database block, the daemon reads the credentials CredentialCandidates
// names, and the full load runs WithCredentials.
//
// The database block never takes a stored value: it opens the store.

// LoadOption changes one load.
type LoadOption func(*loadOptions)

type loadOptions struct {
	credentials map[string]string
	bootstrap   bool
}

func collectLoadOptions(opts []LoadOption) loadOptions {
	var o loadOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// WithCredentials supplies the stored operator credentials, by name, that a
// ${NAME} falls back to when the environment does not hold it.
func WithCredentials(found map[string]string) LoadOption {
	return func(o *loadOptions) { o.credentials = found }
}

// operatorCredentialName is the store's rule for an operator credential's
// name (secretstore.checkOperatorName); a name outside it is never looked up.
var operatorCredentialName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// LoadBootstrap is the first of the daemon's two loads: the configuration
// with its placeholders expanded from the environment only, validated for
// what opening the database and the store needs, and nothing else. A
// required field whose placeholder only the store can fill does not fail
// here; the full load decides.
func LoadBootstrap() (*Config, string, error) {
	configPath := commandLineFlag("config", func() { flag.String("config", "", "Path to configuration file") })
	showVersion := commandLineFlag("version", func() { flag.Bool("version", false, "Show version information") })
	flag.Parse()
	if showVersion.Value.String() == "true" {
		return nil, "", ErrVersionRequested
	}
	path := resolveConfigPath(configPath.Value.String())
	cfg, _, err := LoadFromPathWithProvenance(path, func(o *loadOptions) { o.bootstrap = true })
	if err != nil {
		return nil, path, err
	}
	return cfg, path, nil
}

// LoadBootstrapFromPath is LoadBootstrap for a known path, without flags:
// the reload's first pass.
func LoadBootstrapFromPath(path string) (*Config, error) {
	cfg, _, err := LoadFromPathWithProvenance(path, func(o *loadOptions) { o.bootstrap = true })
	return cfg, err
}

// CredentialCandidates lists the ${NAME}s the configuration references
// outside the database block whose names the store could hold, sorted. The
// daemon looks each up in the store before the full load, including names
// the environment holds, so the load can say when the environment shadows a
// different stored value.
func (c *Config) CredentialCandidates() []string {
	return append([]string(nil), c.credentialCandidates...)
}

// ShadowedCredentials lists the names whose environment value won over a
// different stored value in this load, sorted. Never a value.
func (c *Config) ShadowedCredentials() []string {
	return append([]string(nil), c.shadowedCredentials...)
}

// credentialCandidateNames returns the valid operator-credential names the
// raw config's scalar values reference as ${NAME}, outside the top-level
// database block. Comments and mapping keys are not read (review a3dc F3).
func credentialCandidateNames(raw []byte) []string {
	seen := map[string]bool{}
	for _, n := range placeholderNamesIn(raw, false) {
		if operatorCredentialName.MatchString(n) {
			seen[n] = true
		}
	}
	return sortedNames(seen)
}

// placeholderNamesIn returns the ${NAME}s in the scalar values of raw's
// top-level database block (inDatabase) or of everything else. A document
// that does not parse as a mapping is scanned whole, so in doubt a name
// counts on both sides: a database name then never moves to the store.
func placeholderNamesIn(raw []byte, inDatabase bool) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		var out []string
		for _, m := range placeholderRe.FindAllSubmatch(raw, -1) {
			out = append(out, string(m[1]))
		}
		return out
	}
	var out []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		switch n.Kind {
		case yaml.ScalarNode:
			for _, m := range placeholderRe.FindAllStringSubmatch(n.Value, -1) {
				out = append(out, m[1])
			}
		case yaml.MappingNode:
			for i := 1; i < len(n.Content); i += 2 {
				walk(n.Content[i])
			}
		case yaml.SequenceNode, yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c)
			}
		case yaml.AliasNode:
			if n.Alias != nil {
				walk(n.Alias)
			}
		}
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if (m.Content[i].Value == "database") == inDatabase {
			walk(m.Content[i+1])
		}
	}
	return out
}

// credentialResolver is one load's placeholder mapping, recording which
// names came from the store and which environment values shadowed a
// different stored one.
type credentialResolver struct {
	stored   map[string]string
	fromCred map[string]bool
	shadowed map[string]bool
}

func newCredentialResolver(stored map[string]string) *credentialResolver {
	return &credentialResolver{stored: stored, fromCred: map[string]bool{}, shadowed: map[string]bool{}}
}

// envOnly is the mapping for the database block.
func (r *credentialResolver) envOnly(name string) string { return os.Getenv(name) }

// mapping is the environment, then the stored credential.
func (r *credentialResolver) mapping(name string) string {
	stored, has := r.stored[name]
	if v := os.Getenv(name); v != "" {
		if has && stored != v {
			r.shadowed[name] = true
		}
		return v
	}
	if has && stored != "" {
		r.fromCred[name] = true
		return stored
	}
	return ""
}

func sortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// expandConfig expands every placeholder in cfg: the database block from
// the environment only, everything else through r.mapping. os.Expand makes
// one pass, so a substituted value containing "$" arrives intact.
func expandConfig(cfg *Config, r *credentialResolver) {
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		if !field.CanSet() && field.Kind() != reflect.Struct && field.Kind() != reflect.Map && field.Kind() != reflect.Slice && field.Kind() != reflect.Pointer {
			continue
		}
		mapping := r.mapping
		if t.Field(i).Name == "Database" {
			mapping = r.envOnly
		}
		expandValue(field, mapping)
	}
}

// credentialOrigin classifies a leaf the expansion changed: `credential`
// when one of its names came from the store, `placeholder` otherwise, with
// a note when one of its names shadowed a stored value. Names only.
func (r *credentialResolver) credentialOrigin(was string) (Origin, string) {
	names := placeholderVars(was)
	fromStore, shadows := false, false
	for _, m := range rePlaceholderVar.FindAllStringSubmatch(was, -1) {
		fromStore = fromStore || r.fromCred[m[1]]
		shadows = shadows || r.shadowed[m[1]]
	}
	switch {
	case fromStore:
		return OriginCredential, names
	case shadows:
		return OriginPlaceholder, fmt.Sprintf("%s (shadows a stored credential)", names)
	}
	return OriginPlaceholder, names
}

// DatabaseNames lists the ${NAME}s the raw config's top-level database
// block references in its values, sorted. They open the store, so they stay
// on disk: vornikctl credential import-env never moves one, even when it is
// also referenced elsewhere.
func DatabaseNames(raw []byte) []string {
	seen := map[string]bool{}
	for _, n := range placeholderNamesIn(raw, true) {
		seen[n] = true
	}
	return sortedNames(seen)
}

// ValidCredentialName reports whether the store accepts name.
func ValidCredentialName(name string) bool { return operatorCredentialName.MatchString(name) }
