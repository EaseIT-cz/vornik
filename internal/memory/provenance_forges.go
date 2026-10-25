package memory

// Self-hosted forges for the automatic provenance clones (GitHub #508;
// design 22, "Amendment 2026-10-24 (#508)", F1 to F3). A forge names the
// leading segments of the repository scopes it serves (scope_prefix), the
// https base URL to clone them from, and optionally a daemon-held token that
// only the listed projects may use. A clone records which credential made
// it, and every use checks that the calling project may use it, because a
// verification is a one-bit oracle about the repository's files.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memoryscope"
)

// Forge kinds. Every kind clones; all but ForgeKindGit also answer the
// forge-API verification (F4).
const (
	ForgeKindGitHub  = "github"
	ForgeKindGitLab  = "gitlab"
	ForgeKindGitea   = "gitea"
	ForgeKindForgejo = "forgejo"
	ForgeKindGit     = "git"
)

// DefaultForgeRepoDepth is how many scope segments after the prefix name a
// repository when a forge does not say (owner/repo).
const DefaultForgeRepoDepth = 2

// MaxForgeRepoDepth bounds max_repo_depth (GitLab subgroups).
const MaxForgeRepoDepth = 8

// ProvenanceForge is one configured forge (memory.provenance.forges).
type ProvenanceForge struct {
	Name string
	Kind string
	// Prefix is the scope's leading segments, lower-cased.
	Prefix []string
	// BaseURL is https, with a host and an optional path prefix.
	BaseURL *url.URL
	// Token is the daemon-held credential; "" for anonymous.
	Token string
	// AllowedProjects may use Token; empty means every project.
	AllowedProjects []string
	// MaxRepoDepth is the most segments after Prefix; 0 = DefaultForgeRepoDepth.
	MaxRepoDepth int
}

func (f *ProvenanceForge) allows(projectID string) bool {
	if f == nil || f.Token == "" {
		return false
	}
	if len(f.AllowedProjects) == 0 {
		return true
	}
	for _, p := range f.AllowedProjects {
		if p == projectID {
			return true
		}
	}
	return false
}

// authHeader is the header a clone, fetch or API call sends with the token.
func (f *ProvenanceForge) authHeader() string {
	if f.Token == "" {
		return ""
	}
	if f.Kind == ForgeKindGit {
		return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+f.Token))
	}
	return "Authorization: Bearer " + f.Token
}

// scopeTarget is a scope resolved to a repository the daemon may clone.
type scopeTarget struct {
	key      string   // the lock, failure-memory and directory key
	prefix   []string // host (a default host) or the forge's prefix
	repo     []string // the repository path segments
	forge    *ProvenanceForge
	cloneURL string
}

// github reports whether the target's host is a GitHub (github.com, or a
// configured forge of kind github).
func (t scopeTarget) github() bool {
	if t.forge != nil {
		return t.forge.Kind == ForgeKindGitHub
	}
	return len(t.prefix) == 1 && t.prefix[0] == "github.com"
}

// host is the target's network host.
func (t scopeTarget) host() string {
	if t.forge != nil && t.forge.BaseURL != nil {
		return t.forge.BaseURL.Hostname()
	}
	return t.prefix[0]
}

// resolve maps a normalised scope to its target. reason is "" for a
// cloneable scope; ProvReasonScopeNotCloneable for one that cannot name a
// repository (a bad segment, or a shape no forge and no default host
// accepts), failing closed; ProvReasonCloneFailed for a well-formed
// three-segment scope on a host nobody listed, which may be a forge the
// operator has not configured (unexaminable, as before #508).
func (a *AutoClones) resolve(scope string) (scopeTarget, string) {
	parts := strings.Split(scope, "/")
	for _, p := range parts {
		if p == "." || p == ".." || !autoCloneSegment.MatchString(p) {
			return scopeTarget{}, ProvReasonScopeNotCloneable
		}
	}
	for i := range a.Forges {
		f := &a.Forges[i]
		if !segmentsHavePrefix(parts, f.Prefix) {
			continue
		}
		repo := parts[len(f.Prefix):]
		depth := f.MaxRepoDepth
		if depth <= 0 {
			depth = DefaultForgeRepoDepth
		}
		if len(repo) < 2 || len(repo) > depth || f.BaseURL == nil {
			return scopeTarget{}, ProvReasonScopeNotCloneable
		}
		u := *f.BaseURL
		u.Path = path.Join("/", u.Path, strings.Join(repo, "/")) + ".git"
		u.RawPath, u.RawQuery, u.Fragment, u.User = "", "", "", nil
		return scopeTarget{
			key:    strings.ToLower(strings.Join(f.Prefix, "/")) + "/" + strings.Join(repo, "/"),
			prefix: f.Prefix, repo: repo, forge: f, cloneURL: u.String(),
		}, ""
	}
	if len(parts) != 3 {
		return scopeTarget{}, ProvReasonScopeNotCloneable
	}
	host := strings.ToLower(parts[0])
	if !a.defaultHostListed(host) {
		return scopeTarget{}, ProvReasonCloneFailed
	}
	return scopeTarget{
		key: host + "/" + parts[1] + "/" + parts[2], prefix: []string{host}, repo: parts[1:],
		cloneURL: "https://" + host + "/" + parts[1] + "/" + parts[2] + ".git",
	}, ""
}

func (a *AutoClones) defaultHostListed(host string) bool {
	if a.ReplaceDefaultHosts {
		return false // only the configured forges (air-gapped installs)
	}
	hosts := a.Hosts
	if len(hosts) == 0 {
		hosts = DefaultAutoCloneHosts
	}
	for _, h := range hosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

func segmentsHavePrefix(parts, prefix []string) bool {
	if len(prefix) == 0 || len(parts) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if !strings.EqualFold(parts[i], p) {
			return false
		}
	}
	return true
}

// ExpectedOrigin is the normalised origin a scope's automatic clone must
// have: its clone URL's, which differs from the scope when the forge serves
// the repository on another host form (an SSH-derived scope, an https port).
// "" for a scope with no automatic clone.
func (a *AutoClones) ExpectedOrigin(scope string) string {
	if a == nil {
		return ""
	}
	t, reason := a.resolve(scope)
	if reason != "" {
		return ""
	}
	return memoryscope.NormalizeRemote(t.cloneURL)
}

// dirForTarget is the clone directory: <Root>/<host>/<owner>/<repo> for a
// default host's owner/repo, as before #508; otherwise
// <Root>/<prefix joined by +>/<repo joined by +>, two levels, so no clone
// directory ever lies inside another (segments never hold "+").
func (a *AutoClones) dirForTarget(t scopeTarget) (string, bool) {
	root, err := filepath.Abs(a.Root)
	if err != nil || a.Root == "" {
		return "", false
	}
	var dir string
	if t.forge == nil {
		dir = filepath.Join(root, t.prefix[0], t.repo[0], t.repo[1])
	} else {
		dir = filepath.Join(root, strings.ToLower(strings.Join(t.prefix, "+")), strings.Join(t.repo, "+"))
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) || rel == "." {
		return "", false
	}
	return dir, true
}

// Credential markers: which credential made a clone (F2).
const (
	credentialMarker    = "vornik-provenance-credential"
	credentialAnonymous = "anonymous"
)

func readCredentialMarker(dir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, ".git", credentialMarker))
	if err != nil {
		return "", false
	}
	m := strings.TrimSpace(string(b))
	return m, m != ""
}

func writeCredentialMarker(dir, marker string) error {
	return os.WriteFile(filepath.Join(dir, ".git", credentialMarker), []byte(marker+"\n"), 0o600)
}

// credentialFor picks the credential a project's clone of t is made or
// fetched with (F3): the forge's token when the project may use it, else the
// requesting project's own GitHub App installation for the owner, else
// anonymous. It returns the marker and the header ("" for anonymous).
func (a *AutoClones) credentialFor(ctx context.Context, projectID string, t scopeTarget) (string, string) {
	if t.forge != nil && t.forge.allows(projectID) {
		return "forge:" + t.forge.Name, t.forge.authHeader()
	}
	if t.github() && a.AppToken != nil && projectID != "" {
		if tok, id := a.AppToken(ctx, projectID, t.host(), t.repo[0]); tok != "" && id != "" {
			return "github-app:" + id, githubAppHeader(tok)
		}
	}
	return credentialAnonymous, ""
}

func githubAppHeader(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// mayUse reports whether a project may use a clone made with marker, and the
// header to fetch it with (F2): anonymous is open to every project; a forge
// credential to the projects the forge allows; a GitHub App installation to
// the project whose own App block names it.
func (a *AutoClones) mayUse(ctx context.Context, projectID, marker string, t scopeTarget) (string, bool) {
	switch {
	case marker == credentialAnonymous:
		return "", true
	case strings.HasPrefix(marker, "forge:"):
		if t.forge != nil && "forge:"+t.forge.Name == marker && t.forge.allows(projectID) {
			return t.forge.authHeader(), true
		}
	case strings.HasPrefix(marker, "github-app:"):
		if t.github() && a.AppToken != nil && projectID != "" {
			tok, id := a.AppToken(ctx, projectID, t.host(), t.repo[0])
			if tok != "" && id != "" && "github-app:"+id == marker {
				return githubAppHeader(tok), true
			}
		}
	}
	return "", false
}

// markUnmade records that a scope's clone could not be made (a failed
// creation or a limit), the case the forge API answers instead (F4).
func (a *AutoClones) markUnmade(key string) {
	a.mu.Lock()
	a.unmade[key] = a.now()
	a.mu.Unlock()
}

type sweepCallerKey struct{}

// WithSweepCaller marks the daemon's own provenance sweep, which reads and
// fetches every automatic clone whatever credential made it and compares no
// content. Only the sweeper sets it.
func WithSweepCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, sweepCallerKey{}, true)
}

func sweepCaller(ctx context.Context) bool {
	v, _ := ctx.Value(sweepCallerKey{}).(bool)
	return v
}

// SweepFetch fetches an automatic clone's branch with the credential that
// made it: the forge's token, or a token minted for the recorded GitHub App
// installation (AppTokenByInstallation). A clone whose credential cannot be
// produced is fetched anonymously, which may fail; the ingest path fetches
// it with the project's credential anyway (Ensure).
func (a *AutoClones) SweepFetch(ctx context.Context, scope, dir, branch string) error {
	a.init()
	t, reason := a.resolve(scope)
	if reason != "" {
		return cloneError("scope " + reason)
	}
	header := ""
	if marker, ok := readCredentialMarker(dir); ok {
		switch {
		case strings.HasPrefix(marker, "forge:") && t.forge != nil && marker == "forge:"+t.forge.Name && t.forge.Token != "":
			header = t.forge.authHeader()
		case strings.HasPrefix(marker, "github-app:") && a.AppTokenByIdentity != nil:
			if tok := a.AppTokenByIdentity(ctx, strings.TrimPrefix(marker, "github-app:")); tok != "" {
				header = githubAppHeader(tok)
			}
		}
	}
	err := a.Run.Fetch(ctx, dir, branch, header)
	if err == nil {
		a.mu.Lock()
		a.fetched[t.key] = a.now()
		a.mu.Unlock()
	}
	return err
}

// APIEligible reports whether the forge API may verify a project's version
// in place of a clone (F4): the scope's forge answers the API, its clone
// could not be made (a failed creation or a limit) within the failure
// memory, and the project was not refused an existing clone's credential.
func (a *AutoClones) APIEligible(_ context.Context, _, scope string) bool {
	if a == nil {
		return false
	}
	a.init()
	t, reason := a.resolve(scope)
	if reason != "" || !t.apiKind() {
		return false
	}
	if dir, ok := a.dirForTarget(t); ok && isClone(dir) {
		return false // a clone exists: it answers, or F2 refused this project
	}
	a.mu.Lock()
	at, unmade := a.unmade[t.key]
	a.mu.Unlock()
	return unmade && a.now().Sub(at) < orDefault(a.FailMemory, DefaultAutoCloneFailMemory)
}

// apiKind reports whether the target's forge answers the forge-API
// verification: a configured forge of an API kind, or github.com, gitlab.com
// or codeberg.org (Forgejo).
func (t scopeTarget) apiKind() bool {
	if t.forge != nil {
		return t.forge.Kind != ForgeKindGit
	}
	switch t.prefix[0] {
	case "github.com", "gitlab.com", "codeberg.org":
		return true
	}
	return false
}

// ConfigureAutoClones applies memory.provenance (F1) to the automatic clone
// manager: the one builder the daemon and the operator backfill share. A
// token_file is read here; a forge whose token file cannot be read is an
// error, so a misconfigured credential is reported at start rather than
// surfacing as unverifiable documents.
func ConfigureAutoClones(a *AutoClones, cfg config.MemoryProvenanceConfig, readFile func(string) ([]byte, error)) error {
	if a == nil {
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	a.ReplaceDefaultHosts = cfg.ReplaceDefaultHosts
	a.Forges = nil
	for _, f := range cfg.Forges {
		base, err := url.Parse(strings.TrimSpace(f.BaseURL))
		if err != nil {
			return fmt.Errorf("memory.provenance.forges %s: base_url: %w", f.Name, err)
		}
		base.Path = strings.TrimSuffix(base.Path, "/")
		token := f.Token
		if f.TokenFile != "" {
			b, err := readFile(f.TokenFile)
			if err != nil {
				return fmt.Errorf("memory.provenance.forges %s: token_file: %w", f.Name, err)
			}
			token = strings.TrimSpace(string(b))
		}
		a.Forges = append(a.Forges, ProvenanceForge{
			Name: f.Name, Kind: f.Kind, Prefix: f.PrefixSegments(), BaseURL: base, Token: token,
			AllowedProjects: append([]string(nil), f.AllowedProjects...), MaxRepoDepth: f.MaxRepoDepth,
		})
	}
	return nil
}

// WireForgeVerification gives the verifier the automatic clones' expected
// origins and the forge-API fallback (F1, F4).
func WireForgeVerification(v *ProvenanceVerifier, a *AutoClones) {
	if v == nil || a == nil {
		return
	}
	v.ExpectedOrigin = a.ExpectedOrigin
	v.ForgeAPI = a.ForgeAPIFor
}

// ProvenanceRefusal is one scope the automatic clones refused (F6).
type ProvenanceRefusal struct {
	Scope     string
	ProjectID string
	Reason    string
	Fix       string
	At        time.Time
}

const (
	maxProvenanceRefusals  = 200
	provenanceRefusalReach = 24 * time.Hour
)

// refuse records a refusal for the doctor; the log is in memory, capped, and
// does not survive a restart (F6).
func (a *AutoClones) refuse(projectID, scope, reason, fix string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// One entry per (scope, project, reason), the latest: every ingest of an
	// unlisted repository refuses again, and the doctor lists each once.
	for i, r := range a.refusals {
		if r.Scope == scope && r.ProjectID == projectID && r.Reason == reason {
			a.refusals = append(a.refusals[:i], a.refusals[i+1:]...)
			break
		}
	}
	a.refusals = append(a.refusals, ProvenanceRefusal{Scope: scope, ProjectID: projectID, Reason: reason, Fix: fix, At: a.now()})
	if n := len(a.refusals); n > maxProvenanceRefusals {
		a.refusals = append([]ProvenanceRefusal(nil), a.refusals[n-maxProvenanceRefusals:]...)
	}
}

// Refusals returns the refusals of the last 24 hours, newest last.
func (a *AutoClones) Refusals() []ProvenanceRefusal {
	if a == nil {
		return nil
	}
	a.init()
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []ProvenanceRefusal
	for _, r := range a.refusals {
		if a.now().Sub(r.At) < provenanceRefusalReach {
			out = append(out, r)
		}
	}
	return out
}

// ForgeSummaries describes each configured forge for the doctor: name, kind,
// prefix, base URL and whether a token is set, never the token.
func (a *AutoClones) ForgeSummaries() []string {
	if a == nil {
		return nil
	}
	var out []string
	for _, f := range a.Forges {
		cred := "anonymous"
		if f.Token != "" {
			cred = "token set"
			if len(f.AllowedProjects) > 0 {
				cred += " for " + strings.Join(f.AllowedProjects, ",")
			}
		}
		base := ""
		if f.BaseURL != nil {
			base = f.BaseURL.String()
		}
		out = append(out, fmt.Sprintf("%s (%s, scope_prefix %s, %s, %s)", f.Name, f.Kind, strings.Join(f.Prefix, "/"), base, cred))
	}
	if a.ReplaceDefaultHosts {
		out = append(out, "public default forges dropped (replace_default_hosts)")
	}
	return out
}

// resolveFix is the doctor's fix for a scope resolve refused.
func resolveFix(reason string) string {
	if reason == ProvReasonScopeNotCloneable {
		return "the scope is not <host>/<owner>/<repo>: for a GitLab subgroup or a port, add a memory.provenance.forges entry whose scope_prefix and max_repo_depth match it"
	}
	return "the host is not listed: add a memory.provenance.forges entry with this scope's host as scope_prefix"
}

// firstErrLine is an error's first line, for a refusal. A clone error never
// carries the credential: it travels in GIT_CONFIG_* (runProvenanceCmd).
func firstErrLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// staleForgeMarker reports a clone made with a forge credential that no
// longer applies to its scope (the forge was renamed or removed): such a
// clone is made again rather than refused forever.
func staleForgeMarker(marker string, t scopeTarget) bool {
	if id, ok := strings.CutPrefix(marker, "github-app:"); ok {
		// The installation-id-only form recorded before the App identity
		// (security review of 180ed1da9) names no App: make it again.
		return strings.Count(id, "/") != 2
	}
	if !strings.HasPrefix(marker, "forge:") {
		return false
	}
	return t.forge == nil || marker != "forge:"+t.forge.Name
}
