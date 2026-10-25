package memory

// GitHub #508 (design 22, "Amendment 2026-10-24 (#508)", F1 to F3): a
// self-hosted forge's repositories get automatic provenance clones, with
// the forge's daemon-held token or the requesting project's own GitHub App
// installation, and a clone made with a credential is usable only by a
// project allowed that credential, checked on every use. Found at:
// production (a customer on a self-hosted GitLab gets clone_failed for
// every companion ingest, so nothing ever verifies).

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/config"
)

// recordingRunner is a localCloneRunner that records the URL and header of
// every clone and fetch.
type recordingRunner struct {
	localCloneRunner
	mu      sync.Mutex
	urls    []string
	headers []string
}

func (r *recordingRunner) Clone(ctx context.Context, root, u, dest, header string) error {
	r.mu.Lock()
	r.urls = append(r.urls, u)
	r.headers = append(r.headers, header)
	r.mu.Unlock()
	return r.localCloneRunner.Clone(ctx, root, u, dest, header)
}

func (r *recordingRunner) Fetch(ctx context.Context, dir, branch, header string) error {
	r.mu.Lock()
	r.headers = append(r.headers, header)
	r.mu.Unlock()
	return r.localCloneRunner.Fetch(ctx, dir, branch, header)
}

func (r *recordingRunner) last() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.headers) == 0 {
		return "", ""
	}
	u := ""
	if len(r.urls) > 0 {
		u = r.urls[len(r.urls)-1]
	}
	return u, r.headers[len(r.headers)-1]
}

func corpForge508(t *testing.T, allowed ...string) ProvenanceForge {
	t.Helper()
	base, err := url.Parse("https://gitlab.corp:8443")
	if err != nil {
		t.Fatal(err)
	}
	return ProvenanceForge{
		Name: "corp", Kind: ForgeKindGitLab, Prefix: []string{"gitlab.corp", "8443"},
		BaseURL: base, Token: "glpat-secret-508", AllowedProjects: allowed, MaxRepoDepth: 3,
	}
}

func TestAutoClones508_ConfiguredForgeScopeClonesWithItsToken(t *testing.T) {
	r := &recordingRunner{localCloneRunner: localCloneRunner{origin: newOrigin(t)}}
	a := newAutoClones(t, r)
	a.Forges = []ProvenanceForge{corpForge508(t)}
	ctx := context.Background()

	dir, reason := a.Ensure(ctx, "team-a", "gitlab.corp/8443/group/sub/repo")
	if reason != "" || dir == "" {
		t.Fatalf("Ensure = %q, %q; want a clone (today: scope_not_cloneable)", dir, reason)
	}
	u, h := r.last()
	if u != "https://gitlab.corp:8443/group/sub/repo.git" {
		t.Errorf("clone URL = %q", u)
	}
	if h != "Authorization: Bearer glpat-secret-508" {
		t.Errorf("clone header = %q", h)
	}
	if got := a.ExpectedOrigin("gitlab.corp/8443/group/sub/repo"); got != "gitlab.corp/8443/group/sub/repo" {
		t.Errorf("ExpectedOrigin = %q", got)
	}
	// Deeper than max_repo_depth, or an unlisted 4-segment scope, still fails
	// closed as not a repository.
	if _, reason := a.Ensure(ctx, "team-a", "gitlab.corp/8443/a/b/c/d"); reason != ProvReasonScopeNotCloneable {
		t.Errorf("too deep: reason = %q", reason)
	}
	if _, reason := a.Ensure(ctx, "team-a", "other.corp/group/sub/repo"); reason != ProvReasonScopeNotCloneable {
		t.Errorf("unlisted 4-segment scope: reason = %q", reason)
	}
}

// F2: the clone's credential is checked on every use, not only at creation.
func TestAutoClones508_CredentialIsCheckedOnEveryUse(t *testing.T) {
	r := &recordingRunner{localCloneRunner: localCloneRunner{origin: newOrigin(t)}}
	a := newAutoClones(t, r)
	a.Forges = []ProvenanceForge{corpForge508(t, "team-a")}
	ctx := context.Background()
	scope := "gitlab.corp/8443/group/repo"

	dir, reason := a.Ensure(ctx, "team-a", scope)
	if reason != "" {
		t.Fatalf("allowed project: %q", reason)
	}
	marker, err := os.ReadFile(filepath.Join(dir, ".git", credentialMarker))
	if err != nil || strings.TrimSpace(string(marker)) != "forge:corp" {
		t.Fatalf("marker = %q, %v", marker, err)
	}
	if strings.Contains(string(marker), "glpat") {
		t.Fatal("the token was written to disk")
	}
	if _, reason := a.Ensure(ctx, "team-b", scope); reason != ProvReasonCloneFailed {
		t.Errorf("a project not allowed the forge's token used its clone: reason %q", reason)
	}
	if a.APIEligible(ctx, "team-b", scope) {
		t.Error("an F2 refusal made the project eligible for the forge API")
	}
	if _, reason := a.Ensure(ctx, "team-a", scope); reason != "" {
		t.Errorf("the allowed project lost its clone: %q", reason)
	}
}

// A clone with no marker (made before #508, or tampered) is made again.
func TestAutoClones508_MissingMarkerRemakesTheClone(t *testing.T) {
	r := &recordingRunner{localCloneRunner: localCloneRunner{origin: newOrigin(t)}}
	a := newAutoClones(t, r)
	ctx := context.Background()
	dir, reason := a.Ensure(ctx, "p", "github.com/acme/widgets")
	if reason != "" {
		t.Fatal(reason)
	}
	if err := os.Remove(filepath.Join(dir, ".git", credentialMarker)); err != nil {
		t.Fatal(err)
	}
	before := r.clones.Load()
	if _, reason := a.Ensure(ctx, "p", "github.com/acme/widgets"); reason != "" {
		t.Fatal(reason)
	}
	if r.clones.Load() != before+1 {
		t.Error("a clone without a credential marker was used instead of made again")
	}
}

// F3: only the requesting project's own GitHub App installation is used.
func TestAutoClones508_GitHubAppTokenOfTheRequestingProjectOnly(t *testing.T) {
	r := &recordingRunner{localCloneRunner: localCloneRunner{origin: newOrigin(t)}}
	a := newAutoClones(t, r)
	a.AppToken = func(_ context.Context, projectID, host, owner string) (string, string) {
		switch {
		case projectID == "app-project" && host == "github.com" && strings.EqualFold(owner, "acme"):
			return "ghs_minted508", "api.github.com/1/77"
		case projectID == "borrower" && strings.EqualFold(owner, "acme"):
			// Names the same installation under its own App id.
			return "ghs_borrowed", "api.github.com/2/77"
		}
		return "", ""
	}
	ctx := context.Background()
	if _, reason := a.Ensure(ctx, "app-project", "github.com/Acme/private"); reason != "" {
		t.Fatal(reason)
	}
	_, h := r.last()
	if !strings.HasPrefix(h, "Authorization: Basic ") || h == "Authorization: Basic " {
		t.Errorf("App clone header = %q", h)
	}
	if _, reason := a.Ensure(ctx, "other-project", "github.com/Acme/private"); reason != ProvReasonCloneFailed {
		t.Errorf("another project used the App-made clone: %q", reason)
	}
	// Security review of 180ed1da9: the same installation id under another
	// App identity is a different credential.
	if _, reason := a.Ensure(ctx, "borrower", "github.com/Acme/private"); reason != ProvReasonCloneFailed {
		t.Errorf("a project naming the installation under another App used the clone: %q", reason)
	}
}

func TestAutoClones508_ReplaceDefaultHosts(t *testing.T) {
	r := &recordingRunner{localCloneRunner: localCloneRunner{origin: newOrigin(t)}}
	a := newAutoClones(t, r)
	a.Forges = []ProvenanceForge{corpForge508(t)}
	a.ReplaceDefaultHosts = true
	if _, reason := a.Ensure(context.Background(), "p", "github.com/acme/widgets"); reason != ProvReasonCloneFailed {
		t.Errorf("a default host was cloned with replace_default_hosts: %q", reason)
	}
}

// The origin the verifier expects is the clone URL's, which differs from the
// scope when the scope came from an SSH remote and the forge serves https on
// a port.
func TestAutoClones508_ExpectedOriginFollowsTheCloneURL(t *testing.T) {
	a := newAutoClones(t, &recordingRunner{})
	f := corpForge508(t)
	f.Prefix = []string{"gitlab.corp"}
	f.MaxRepoDepth = 2
	a.Forges = []ProvenanceForge{f}
	if got := a.ExpectedOrigin("gitlab.corp/group/repo"); got != "gitlab.corp/8443/group/repo" {
		t.Errorf("ExpectedOrigin = %q", got)
	}
	if got := a.ExpectedOrigin("github.com/acme/widgets"); got != "github.com/acme/widgets" {
		t.Errorf("default host ExpectedOrigin = %q", got)
	}
}

func TestAutoClones508_ListsBothLayouts(t *testing.T) {
	r := &recordingRunner{localCloneRunner: localCloneRunner{origin: newOrigin(t)}}
	a := newAutoClones(t, r)
	a.Forges = []ProvenanceForge{corpForge508(t)}
	ctx := context.Background()
	for _, s := range []string{"github.com/acme/widgets", "gitlab.corp/8443/group/sub/repo"} {
		if _, reason := a.Ensure(ctx, "p", s); reason != "" {
			t.Fatal(s, reason)
		}
	}
	got := map[string]bool{}
	for _, c := range a.List() {
		got[c.RepoScope] = true
	}
	for _, s := range []string{"github.com/acme/widgets", "gitlab.corp/8443/group/sub/repo"} {
		if !got[s] {
			t.Errorf("List misses %q: %v", s, got)
		}
	}
}

// The daemon's sweep fetches an automatic clone made with a forge token with
// that token, although no project asks (a sweep compares no content). Before
// #508 F2, the sweep resolved the clone with no project, which a credentialed
// clone now refuses.
func TestProvenanceSweep508_FetchesACredentialedCloneWithItsToken(t *testing.T) {
	r := &recordingRunner{localCloneRunner: localCloneRunner{origin: newOrigin(t)}}
	a := newAutoClones(t, r)
	a.Forges = []ProvenanceForge{corpForge508(t, "team-a")}
	ctx := context.Background()
	scope := "gitlab.corp/8443/group/repo"
	dir, reason := a.Ensure(ctx, "team-a", scope)
	if reason != "" {
		t.Fatal(reason)
	}
	// The verifier sees the forge's clone URL as the origin.
	realGit := SpawnGitRunner
	v := NewProvenanceVerifier(t.TempDir())
	v.CloneFor = ScopedCloneFor(func(string) (string, string, bool) { return "", "", false }, a)
	v.ExpectedOrigin = a.ExpectedOrigin
	v.Git = func(ctx context.Context, d string, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "remote" && args[1] == "get-url" {
			return []byte("https://gitlab.corp:8443/group/repo.git\n"), nil
		}
		return realGit(ctx, d, args...)
	}
	s := &ProvenanceSweeper{Verifier: v, Clones: a.List, AutoFetch: a.SweepFetch}
	before := len(r.headers)
	s.FetchClones(ctx)
	got := s.LastFetches()
	if len(got) != 1 || got[0].Outcome != ProvenanceFetchOK || got[0].Dir != dir {
		t.Fatalf("sweep fetch = %+v", got)
	}
	if len(r.headers) != before+1 || r.headers[len(r.headers)-1] != "Authorization: Bearer glpat-secret-508" {
		t.Errorf("sweep fetch headers = %q", r.headers[before:])
	}
}

func TestConfigureAutoClones508(t *testing.T) {
	a := &AutoClones{}
	err := ConfigureAutoClones(a, config.MemoryProvenanceConfig{ReplaceDefaultHosts: true, Forges: []config.ProvenanceForgeConfig{{
		Name: "corp", Kind: "gitea", ScopePrefix: "Git.Corp", BaseURL: "https://git.corp/forge/", TokenFile: "/run/t",
		AllowedProjects: []string{"a"}, MaxRepoDepth: 3,
	}}}, func(p string) ([]byte, error) {
		if p != "/run/t" {
			t.Fatalf("read %q", p)
		}
		return []byte("tok-from-file\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !a.ReplaceDefaultHosts || len(a.Forges) != 1 {
		t.Fatalf("a = %+v", a)
	}
	f := a.Forges[0]
	if f.Token != "tok-from-file" || f.BaseURL.String() != "https://git.corp/forge" || strings.Join(f.Prefix, "/") != "git.corp" {
		t.Errorf("forge = %+v (%s)", f, f.BaseURL)
	}
	if got := a.ExpectedOrigin("git.corp/acme/repo"); got != "git.corp/forge/acme/repo" {
		t.Errorf("ExpectedOrigin = %q", got)
	}
	if err := ConfigureAutoClones(&AutoClones{}, config.MemoryProvenanceConfig{Forges: []config.ProvenanceForgeConfig{{
		Name: "x", Kind: "git", ScopePrefix: "h", BaseURL: "https://h", TokenFile: "/missing",
	}}}, func(string) ([]byte, error) { return nil, os.ErrNotExist }); err == nil {
		t.Error("an unreadable token_file was accepted")
	}
}

// F6: the doctor names each configured forge (never its token) and each scope
// refused recently, with the reason and the fix.
func TestAutoClones508_DoctorNotes(t *testing.T) {
	r := &recordingRunner{localCloneRunner: localCloneRunner{origin: newOrigin(t)}}
	a := newAutoClones(t, r)
	a.Forges = []ProvenanceForge{corpForge508(t, "team-a")}
	ctx := context.Background()
	a.Ensure(ctx, "team-a", "gitlab.corp/8443/group/repo")
	a.Ensure(ctx, "team-b", "gitlab.corp/8443/group/repo")
	a.Ensure(ctx, "team-a", "git.unlisted.example/acme/repo")
	a.Ensure(ctx, "team-a", "git.unlisted.example/a/b/c")
	a.Ensure(ctx, "team-a", "git.unlisted.example/a/b/c") // listed once

	forges := strings.Join(a.ForgeSummaries(), "\n")
	if !strings.Contains(forges, "corp") || !strings.Contains(forges, "gitlab.corp/8443") || strings.Contains(forges, "glpat") {
		t.Errorf("forge summaries = %q", forges)
	}
	ref := a.Refusals()
	if len(ref) != 3 {
		t.Errorf("refusals = %d, want 3 (one per scope, project and reason)", len(ref))
	}
	text := ""
	for _, x := range ref {
		text += x.Scope + " | " + x.Reason + " | " + x.Fix + "\n"
	}
	for _, want := range []string{
		"git.unlisted.example/acme/repo", "memory.provenance.forges",
		"team-b", "allowed_projects",
		"git.unlisted.example/a/b/c",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("refusals lack %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "glpat") {
		t.Error("a refusal carries the token")
	}
}

// A clone whose marker names a forge the configuration no longer has for its
// scope (renamed or removed) is stale: made again, never refused forever.
func TestAutoClones508_StaleForgeMarkerRemakes(t *testing.T) {
	r := &recordingRunner{localCloneRunner: localCloneRunner{origin: newOrigin(t)}}
	a := newAutoClones(t, r)
	a.Forges = []ProvenanceForge{corpForge508(t, "team-a")}
	ctx := context.Background()
	scope := "gitlab.corp/8443/group/repo"
	if _, reason := a.Ensure(ctx, "team-a", scope); reason != "" {
		t.Fatal(reason)
	}
	a.Forges[0].Name = "corp-renamed"
	before := r.clones.Load()
	if _, reason := a.Ensure(ctx, "team-a", scope); reason != "" {
		t.Fatalf("renamed forge: %q", reason)
	}
	if r.clones.Load() != before+1 {
		t.Error("a clone made by a forge that no longer exists was not made again")
	}
}

// The sweep never re-makes a clone (a marker-less clone from before #508 is
// re-made on its next project use), so an upgrade does not re-clone every
// repository in one pass.
func TestAutoClones508_SweepDoesNotRemake(t *testing.T) {
	r := &recordingRunner{localCloneRunner: localCloneRunner{origin: newOrigin(t)}}
	a := newAutoClones(t, r)
	ctx := context.Background()
	dir, reason := a.Ensure(ctx, "p", "github.com/acme/widgets")
	if reason != "" {
		t.Fatal(reason)
	}
	if err := os.Remove(filepath.Join(dir, ".git", credentialMarker)); err != nil {
		t.Fatal(err)
	}
	before := r.clones.Load()
	if _, reason := a.Ensure(WithSweepCaller(ctx), "", "github.com/acme/widgets"); reason != ProvReasonNoClone {
		t.Errorf("sweep on a marker-less clone: reason %q", reason)
	}
	if r.clones.Load() != before {
		t.Error("the sweep re-made a clone")
	}
}

// An App marker in the installation-only form (before the App identity) is
// stale: the clone is made again rather than refused forever.
func TestAutoClones508_InstallationOnlyAppMarkerIsStale(t *testing.T) {
	tgt := scopeTarget{prefix: []string{"github.com"}, repo: []string{"a", "b"}}
	if !staleForgeMarker("github-app:77", tgt) {
		t.Error("the installation-only marker was trusted")
	}
	if staleForgeMarker("github-app:api.github.com/1/77", tgt) || staleForgeMarker(credentialAnonymous, tgt) {
		t.Error("a current marker was treated as stale")
	}
}

// credentialOnlyRunner refuses an anonymous clone, as a private repository
// does, and lets a credentialed one through.
type credentialOnlyRunner struct{ recordingRunner }

func (r *credentialOnlyRunner) Clone(ctx context.Context, root, u, dest, header string) error {
	if header == "" {
		r.mu.Lock()
		r.urls = append(r.urls, u)
		r.headers = append(r.headers, header)
		r.mu.Unlock()
		return errors.New("fatal: could not read Username for 'https://github.com': terminal prompts disabled")
	}
	return r.recordingRunner.Clone(ctx, root, u, dest, header)
}

// Push security review of 180ed1da9 (2026-10-25): the failure memory was keyed
// by scope alone, so a project with no credential for a private repository
// blocked, for an hour, the clone a project WITH a credential would have made.
func TestAutoClones508_FailedAnonymousCloneDoesNotBlockACredentialedProject(t *testing.T) {
	r := &credentialOnlyRunner{recordingRunner{localCloneRunner: localCloneRunner{origin: newOrigin(t)}}}
	a := newAutoClones(t, r)
	a.AppToken = func(_ context.Context, projectID, _, owner string) (string, string) {
		if projectID == "app-project" && strings.EqualFold(owner, "acme") {
			return "ghs_minted508", "api.github.com/1/77"
		}
		return "", ""
	}
	ctx := context.Background()
	if _, reason := a.Ensure(ctx, "anon-project", "github.com/Acme/private"); reason != ProvReasonCloneFailed {
		t.Fatalf("anonymous clone of a private repository: reason %q, want %q", reason, ProvReasonCloneFailed)
	}
	if _, reason := a.Ensure(ctx, "app-project", "github.com/Acme/private"); reason != "" {
		t.Fatalf("a project with a GitHub App credential was refused after another project's anonymous failure: %q", reason)
	}
	// The failure memory still spares the anonymous credential a retry.
	if _, reason := a.Ensure(ctx, "anon-project-2", "github.com/Acme/other"); reason != ProvReasonCloneFailed {
		t.Fatalf("setup: second scope anonymous attempt: %q", reason)
	}
	before := len(r.urls)
	if _, reason := a.Ensure(ctx, "anon-project-2", "github.com/Acme/other"); reason != ProvReasonCloneFailed {
		t.Fatalf("anonymous retry within the failure memory: %q", reason)
	}
	if len(r.urls) != before {
		t.Errorf("the failure memory did not spare an anonymous retry (%d clone attempts, want %d)", len(r.urls), before)
	}
}
