package config

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// MemoryProvenanceConfig configures provenance verification of
// companion-delegated documents beyond the public forges (GitHub #508;
// design 22, "Amendment 2026-10-24 (#508)", F1).
type MemoryProvenanceConfig struct {
	// ReplaceDefaultHosts drops github.com, gitlab.com, bitbucket.org and
	// codeberg.org, leaving only Forges (air-gapped installs).
	ReplaceDefaultHosts bool `yaml:"replace_default_hosts" json:"replace_default_hosts" doc:"Drop the public default forges (github.com, gitlab.com, bitbucket.org, codeberg.org); only memory.provenance.forges are cloned." since:"2026.10.8"`
	// Forges are self-hosted forges whose repositories may be cloned (and,
	// for an API kind, checked through their API) to verify a document.
	Forges []ProvenanceForgeConfig `yaml:"forges" json:"forges" doc:"Self-hosted forges for provenance clones: name, kind (github, gitlab, gitea, forgejo, git), scope_prefix, base_url, token or token_file, allowed_projects, max_repo_depth." since:"2026.10.8"`
}

// ProvenanceForgeConfig is one memory.provenance.forges entry.
type ProvenanceForgeConfig struct {
	Name string `yaml:"name" json:"name"`
	Kind string `yaml:"kind" json:"kind"`
	// ScopePrefix is the leading segments of the repository scopes this forge
	// serves, as memoryscope.NormalizeRemote writes them (a port becomes a
	// segment: gitlab.corp/8443).
	ScopePrefix string `yaml:"scope_prefix" json:"scope_prefix"`
	// BaseURL is the https URL repositories are cloned under.
	BaseURL string `yaml:"base_url" json:"base_url"`
	// Token is a read-only forge token (${VAR} expanded at load). Never
	// serialised: json "-" keeps it out of config dumps.
	Token string `yaml:"token" json:"-"`
	// TokenFile is an absolute path read at daemon start instead of Token.
	TokenFile string `yaml:"token_file" json:"token_file"`
	// AllowedProjects may use the token; empty = every project.
	AllowedProjects []string `yaml:"allowed_projects" json:"allowed_projects"`
	// MaxRepoDepth is the most scope segments after the prefix (2 to 8;
	// 0 = 2, owner/repo). GitLab subgroups need more.
	MaxRepoDepth int `yaml:"max_repo_depth" json:"max_repo_depth"`
}

// provenanceSegment is the scope segment rule the automatic clones use.
var provenanceSegment = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// PrefixSegments splits ScopePrefix into lower-cased segments.
func (f ProvenanceForgeConfig) PrefixSegments() []string {
	var out []string
	for _, s := range strings.Split(strings.Trim(strings.TrimSpace(f.ScopePrefix), "/"), "/") {
		out = append(out, strings.ToLower(s))
	}
	return out
}

// Validate refuses a forge the verifier could not use safely.
func (c MemoryProvenanceConfig) Validate() error {
	names := map[string]bool{}
	var prefixes [][]string
	for i, f := range c.Forges {
		where := fmt.Sprintf("memory.provenance.forges[%d]", i)
		if strings.TrimSpace(f.Name) == "" || !provenanceSegment.MatchString(f.Name) {
			return fmt.Errorf("%s: name must be a plain identifier", where)
		}
		if names[f.Name] {
			return fmt.Errorf("%s: duplicate forge name %q", where, f.Name)
		}
		names[f.Name] = true
		where += " (" + f.Name + ")"
		segs := f.PrefixSegments()
		for _, other := range prefixes {
			if segmentPrefixOf(segs, other) || segmentPrefixOf(other, segs) {
				return fmt.Errorf("%s: scope_prefix %q overlaps another forge's", where, f.ScopePrefix)
			}
		}
		prefixes = append(prefixes, segs)
		if err := f.validate(where, segs); err != nil {
			return err
		}
	}
	return nil
}

// validate checks one forge's own fields.
func (f ProvenanceForgeConfig) validate(where string, segs []string) error {
	switch f.Kind {
	case "github", "gitlab", "gitea", "forgejo", "git":
	default:
		return fmt.Errorf("%s: kind must be github, gitlab, gitea, forgejo or git", where)
	}
	for _, s := range segs {
		if s == "." || s == ".." || !provenanceSegment.MatchString(s) {
			return fmt.Errorf("%s: scope_prefix %q is not segments of [A-Za-z0-9._-]", where, f.ScopePrefix)
		}
	}
	if !plainHTTPSBase(f.BaseURL) {
		return fmt.Errorf("%s: base_url must be a plain https URL (no userinfo, query, fragment or dot segments)", where)
	}
	if f.MaxRepoDepth != 0 && (f.MaxRepoDepth < 2 || f.MaxRepoDepth > 8) {
		return fmt.Errorf("%s: max_repo_depth must be 2 to 8", where)
	}
	if f.Token != "" && f.TokenFile != "" {
		return fmt.Errorf("%s: set token or token_file, not both", where)
	}
	if f.TokenFile != "" && !filepath.IsAbs(f.TokenFile) {
		return fmt.Errorf("%s: token_file must be an absolute path", where)
	}
	return nil
}

// plainHTTPSBase accepts an https URL with a host and an optional clean path,
// and nothing else.
func plainHTTPSBase(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	return u.Path == "" || path.Clean(u.Path) == strings.TrimSuffix(u.Path, "/")
}

// segmentPrefixOf reports whether a is a leading run of b's segments.
func segmentPrefixOf(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
