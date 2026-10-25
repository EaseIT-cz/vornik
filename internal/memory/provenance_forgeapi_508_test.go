package memory

// GitHub #508 (design 22, "Amendment 2026-10-24 (#508)"): the verifier
// accepts an automatic clone whose origin is the forge's clone URL rather
// than the scope (an SSH-derived scope, an https port), and verifies through
// the forge API when no clone can be made (F4), stamping
// provenance:forge-api.

import (
	"context"
	"testing"
)

func TestProvenance508_ExpectedOriginIsAccepted(t *testing.T) {
	f := newProvFixture(t)
	sshScope := "gitlab.corp/group/repo"
	f.v.CloneFor = func(context.Context, string, string) (string, string) { return f.clone, "" }
	r := f.v.Verify(context.Background(), ProvenanceRequest{ProjectID: f.project, RepoScope: sshScope, Path: provLLD, Content: []byte(provLLDBody)})
	wantReason(t, r, ProvenanceUnverified, ProvReasonRepoMismatch)

	f.v.ExpectedOrigin = func(scope string) string {
		if scope == sshScope {
			return f.scope
		}
		return ""
	}
	r = f.v.Verify(context.Background(), ProvenanceRequest{ProjectID: f.project, RepoScope: sshScope, Path: provLLD, Content: []byte(provLLDBody)})
	wantReason(t, r, ProvenanceVerified, "")
	if r.Role() != ValidatorRoleProvenance {
		t.Errorf("clone role = %q", r.Role())
	}
}

// fakeForgeAPI answers from a map of path -> (blob id, kind).
type fakeForgeAPI struct {
	branch      string
	head        string
	files       map[string][2]string
	unavailable bool
}

func (f fakeForgeAPI) DefaultBranch(context.Context) (string, error) {
	if f.unavailable {
		return "", ErrForgeUnavailable
	}
	return f.branch, nil
}

func (f fakeForgeAPI) BranchHead(_ context.Context, branch string) (string, error) {
	if f.unavailable {
		return "", ErrForgeUnavailable
	}
	if branch != f.branch {
		return "", ErrForgeNotFound
	}
	return f.head, nil
}

func (f fakeForgeAPI) BlobAt(_ context.Context, _, p string) (string, string, error) {
	if f.unavailable {
		return "", "", ErrForgeUnavailable
	}
	e, ok := f.files[p]
	if !ok {
		return "", "", ErrForgeNotFound
	}
	return e[0], e[1], nil
}

func TestProvenance508_ForgeAPIVerifies(t *testing.T) {
	const scope = "gitlab.corp/8443/group/repo"
	body := []byte(provLLDBody)
	api := fakeForgeAPI{branch: "main", head: "1111111111111111111111111111111111111111", files: map[string][2]string{
		provLLD:     {gitBlobID(body, false), ForgeEntryFile},
		"CLAUDE.md": {gitBlobID([]byte("other"), false), ForgeEntryFile},
		"docs/link": {gitBlobID(body, false), "symlink"},
	}}
	newV := func(a ForgeAPIClient, eligible bool) *ProvenanceVerifier {
		v := NewProvenanceVerifier(t.TempDir())
		v.CloneFor = func(context.Context, string, string) (string, string) { return "", ProvReasonCloneFailed }
		v.ForgeAPI = func(context.Context, string, string) (ForgeAPIClient, bool) { return a, eligible }
		return v
	}
	verify := func(v *ProvenanceVerifier, p string, content []byte) ProvenanceResult {
		return v.Verify(context.Background(), ProvenanceRequest{ProjectID: "p", RepoScope: scope, Path: p, Content: content})
	}

	r := verify(newV(api, true), provLLD, body)
	wantReason(t, r, ProvenanceVerified, "")
	if r.Role() != ValidatorRoleProvenanceForgeAPI || r.CommitSHA != api.head || r.Repo != scope {
		t.Errorf("API result = %+v role %q", r, r.Role())
	}
	wantReason(t, verify(newV(api, true), "CLAUDE.md", body), ProvenanceUnverified, ProvReasonBlobMismatch)
	wantReason(t, verify(newV(api, true), "docs/missing.md", body), ProvenanceUnverified, ProvReasonNotFound)
	wantReason(t, verify(newV(api, true), "docs/link", body), ProvenanceUnverified, ProvReasonNotRegularFile)
	// An unreachable or refusing API is unexaminable, never unverified.
	wantReason(t, verify(newV(fakeForgeAPI{unavailable: true}, true), provLLD, body), ProvenanceUnverified, ProvReasonCloneFailed)
	// Not eligible (no API kind, or F2 refused the project): the clone's reason stands.
	wantReason(t, verify(newV(api, false), provLLD, body), ProvenanceUnverified, ProvReasonCloneFailed)
	// The branch the project configured, absent on the forge: examined.
	v := newV(api, true)
	v.DefaultBranchFor = func(string, string) string { return "release" }
	wantReason(t, verify(v, provLLD, body), ProvenanceUnverified, ProvReasonNoOriginMain)
}

// A re-check of an API-verified record whose file changed is stale, as on the
// clone path.
func TestProvenance508_ForgeAPIRecheckGoesStale(t *testing.T) {
	api := fakeForgeAPI{branch: "main", head: "2222222222222222222222222222222222222222", files: map[string][2]string{
		provLLD: {gitBlobID([]byte("changed"), false), ForgeEntryFile},
	}}
	v := NewProvenanceVerifier(t.TempDir())
	v.CloneFor = func(context.Context, string, string) (string, string) { return "", ProvReasonCloneFailed }
	v.ForgeAPI = func(context.Context, string, string) (ForgeAPIClient, bool) { return api, true }
	r := v.Recheck(context.Background(), ProvenancePrior{ProjectID: "p", RepoScope: "gitlab.corp/8443/g/r", Path: provLLD,
		State: ProvenanceVerified, CommitSHA: "1111111111111111111111111111111111111111", ContentBlobSHA: gitBlobID([]byte(provLLDBody), false)})
	wantReason(t, r, ProvenanceStale, ProvReasonBlobChanged)
}

// sameRoles compares role lists in order (#508 F5: the sweep withdraws either
// provenance stamp).
func sameRoles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIsProvenanceRole508(t *testing.T) {
	for role, want := range map[string]bool{
		ValidatorRoleProvenance: true, ValidatorRoleProvenanceForgeAPI: true,
		ValidatorRoleProvenanceUnavailable: false, "rag-ingester": false, "": false,
	} {
		if IsProvenanceRole(role) != want {
			t.Errorf("IsProvenanceRole(%q) != %v", role, want)
		}
	}
	if got := ProvenanceLabel("verified", ValidatorRoleProvenanceForgeAPI, ProvenanceVerified, ""); got != "verified (forge API)" {
		t.Errorf("label = %q", got)
	}
	if got := provenanceRoleSQL("c.validator_role"); got != "COALESCE(c.validator_role, '') IN ('provenance:origin/main', 'provenance:forge-api')" {
		t.Errorf("SQL = %q", got)
	}
}
