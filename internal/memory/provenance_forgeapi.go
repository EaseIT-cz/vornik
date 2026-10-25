package memory

// Forge-API verification (GitHub #508; design 22, "Amendment 2026-10-24
// (#508)", F4). When a scope's clone cannot be made (a failed creation, or a
// repository past the clone size limit), a forge that answers a contents API
// reports the blob id of the path at the trusted branch's head, and the
// verifier compares it with the stored bytes' blob id. The trust is the
// clone's (operator decision 2026-10-24): both rest on the forge's answer
// over TLS.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"vornik.io/vornik/internal/integrations/dialguard"
)

// ForgeAPIClient is what the verifier needs of a forge's API.
type ForgeAPIClient interface {
	// DefaultBranch reads the repository; an error other than
	// ErrForgeNotFound means the repository could not be examined.
	DefaultBranch(ctx context.Context) (string, error)
	// BranchHead is the branch's head commit; ErrForgeNotFound when the
	// branch does not exist.
	BranchHead(ctx context.Context, branch string) (string, error)
	// BlobAt is the path's blob id at a commit and its kind (ForgeEntryFile
	// for a regular file); ErrForgeNotFound when the path does not exist.
	BlobAt(ctx context.Context, commit, p string) (blobID, kind string, err error)
}

// ForgeEntryFile is a regular file's kind in BlobAt.
const ForgeEntryFile = "file"

var (
	// ErrForgeNotFound is an examined absence: the branch or path is not there.
	ErrForgeNotFound = errors.New("forge: not found")
	// ErrForgeUnavailable is a forge the daemon could not read: unreachable,
	// refused (401/403), rate-limited or failing (5xx). Unexaminable.
	ErrForgeUnavailable = errors.New("forge: unavailable")
)

// evaluateAPI is evaluate's rule through the forge API: the repository must
// be readable (else unexaminable, clone_failed); the trusted branch (the
// project's configured one for its own scope, else the forge's default) must
// exist; the path must be a regular file at its head with the stored bytes'
// blob id. A re-check of a verified record compares at the current head (no
// ancestry walk); a changed blob is stale, as on the clone path.
func (v *ProvenanceVerifier) evaluateAPI(ctx context.Context, api ForgeAPIClient, projectID, scope, p string,
	contentBlob func(sha256Repo bool) string, prior ProvenancePrior, res ProvenanceResult) ProvenanceResult {
	fail := func(reason string) ProvenanceResult {
		res.State, res.Reason = ProvenanceUnverified, reason
		return res
	}
	res.Repo = scope
	def, err := api.DefaultBranch(ctx)
	if err != nil {
		return fail(ProvReasonCloneFailed)
	}
	branch := def
	if v.DefaultBranchFor != nil {
		if b := v.DefaultBranchFor(projectID, scope); b != "" {
			branch = b
		}
	}
	if !plainBranchName(branch) {
		return fail(ProvReasonNoOriginMain)
	}
	head, err := api.BranchHead(ctx, branch)
	switch {
	case errors.Is(err, ErrForgeNotFound):
		return fail(ProvReasonNoOriginMain)
	case err != nil:
		return fail(ProvReasonCloneFailed)
	case !isHexID(head):
		return fail(ProvReasonNoOriginMain)
	}
	res.ContentBlobSHA = contentBlob(len(head) == 64)
	blob, kind, err := api.BlobAt(ctx, head, p)
	wasVerified := prior.State == ProvenanceVerified || prior.State == ProvenanceStale
	stale := func() ProvenanceResult {
		res.State, res.Reason, res.CommitSHA = ProvenanceStale, ProvReasonBlobChanged, prior.CommitSHA
		return res
	}
	switch {
	case errors.Is(err, ErrForgeNotFound):
		if wasVerified {
			return stale()
		}
		return fail(ProvReasonNotFound)
	case err != nil:
		return fail(ProvReasonCloneFailed)
	case kind != ForgeEntryFile:
		if wasVerified {
			return stale()
		}
		return fail(ProvReasonNotRegularFile)
	}
	res.OriginBlobSHA = blob
	if blob != res.ContentBlobSHA {
		if wasVerified {
			return stale()
		}
		return fail(ProvReasonBlobMismatch)
	}
	res.State, res.Reason, res.CommitSHA, res.Method = ProvenanceVerified, "", head, ProvenanceMethodForgeAPI
	return res
}

// Forge API limits (F4).
const (
	forgeAPITimeout   = 20 * time.Second
	forgeAPIBodyLimit = 1 << 20
	forgeAPISlots     = 4
)

var forgeAPISem = make(chan struct{}, forgeAPISlots)

// forgeHTTPAPI is a ForgeAPIClient over one repository of one forge.
type forgeHTTPAPI struct {
	kind   string
	api    url.URL  // the API root (…/api/v4, …/api/v1, api.github.com or …/api/v3)
	repo   []string // the repository path segments
	header string   // "Authorization: …", or "" for anonymous
	client *http.Client
}

// newForgeHTTPAPI builds the client for a target with the credential header
// the project may use. ok is false for a target with no API (kind git, or a
// default host other than github.com, gitlab.com or codeberg.org).
func newForgeHTTPAPI(t scopeTarget, header string) (*forgeHTTPAPI, bool) {
	if !t.apiKind() {
		return nil, false
	}
	kind := ""
	var base url.URL
	if t.forge != nil {
		kind, base = t.forge.Kind, *t.forge.BaseURL
	} else {
		switch t.prefix[0] {
		case "github.com":
			kind = ForgeKindGitHub
		case "gitlab.com":
			kind = ForgeKindGitLab
		case "codeberg.org":
			kind = ForgeKindForgejo
		}
		base = url.URL{Scheme: "https", Host: t.prefix[0]}
	}
	api := base
	api.RawQuery, api.Fragment, api.User, api.RawPath = "", "", nil, ""
	switch kind {
	case ForgeKindGitHub:
		if t.forge == nil {
			api = url.URL{Scheme: "https", Host: "api.github.com"}
		} else {
			api.Path = path.Join("/", base.Path, "api/v3")
		}
	case ForgeKindGitLab:
		api.Path = path.Join("/", base.Path, "api/v4")
	case ForgeKindGitea, ForgeKindForgejo:
		api.Path = path.Join("/", base.Path, "api/v1")
	default:
		return nil, false
	}
	guard := dialguard.DialGuard{AllowedHosts: []string{api.Hostname()}}
	return &forgeHTTPAPI{kind: kind, api: api, repo: t.repo, header: header, client: guard.NoRedirectHTTPClient(forgeAPITimeout)}, true
}

func (f *forgeHTTPAPI) repoURL(suffix ...string) url.URL {
	u := f.api
	if f.kind == ForgeKindGitLab {
		// GitLab names a project by its URL-encoded full path.
		u.Path = path.Join(f.api.Path, "projects", strings.Join(f.repo, "/"))
		u.RawPath = path.Join(f.api.Path, "projects", url.PathEscape(strings.Join(f.repo, "/")))
		for _, s := range suffix {
			u.Path = path.Join(u.Path, s)
			u.RawPath = path.Join(u.RawPath, url.PathEscape(s))
		}
		return u
	}
	parts := append([]string{f.api.Path, "repos"}, f.repo...)
	u.Path = path.Join(append(parts, suffix...)...)
	return u
}

// get fetches one JSON document into out.
func (f *forgeHTTPAPI) get(ctx context.Context, u url.URL, out any) error {
	select {
	case forgeAPISem <- struct{}{}:
	case <-ctx.Done():
		return ErrForgeUnavailable
	}
	defer func() { <-forgeAPISem }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ErrForgeUnavailable
	}
	req.Header.Set("Accept", "application/json")
	if name, value, ok := strings.Cut(f.header, ": "); ok && f.header != "" {
		req.Header.Set(name, value)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return ErrForgeUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrForgeNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: HTTP %d", ErrForgeUnavailable, resp.StatusCode)
	}
	body, err := dialguard.ReadBoundedBody(resp.Body, forgeAPIBodyLimit)
	if err != nil {
		return ErrForgeUnavailable
	}
	if err := json.Unmarshal(body, out); err != nil {
		return ErrForgeUnavailable
	}
	return nil
}

// DefaultBranch reads the repository. A 404 here means the daemon cannot see
// the repository (GitHub answers 404 for a private repository without
// access), so it is unavailable, not an examined absence.
func (f *forgeHTTPAPI) DefaultBranch(ctx context.Context) (string, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := f.get(ctx, f.repoURL(), &repo); err != nil {
		return "", ErrForgeUnavailable
	}
	if repo.DefaultBranch == "" {
		return "", ErrForgeUnavailable
	}
	return repo.DefaultBranch, nil
}

func (f *forgeHTTPAPI) BranchHead(ctx context.Context, branch string) (string, error) {
	var b struct {
		Commit struct {
			SHA string `json:"sha"` // GitHub
			ID  string `json:"id"`  // GitLab, Gitea, Forgejo
		} `json:"commit"`
	}
	seg := "branches"
	if f.kind == ForgeKindGitLab {
		seg = "repository/branches"
	}
	u := f.repoURL(strings.Split(seg, "/")...)
	u.Path = path.Join(u.Path, branch)
	if u.RawPath != "" {
		u.RawPath = path.Join(u.RawPath, url.PathEscape(branch))
	}
	if err := f.get(ctx, u, &b); err != nil {
		return "", err
	}
	if b.Commit.SHA != "" {
		return b.Commit.SHA, nil
	}
	return b.Commit.ID, nil
}

// BlobAt reads the path's entry at commit. GitHub and Gitea/Forgejo report
// the entry type (file, symlink, dir, submodule); GitLab's files API reports
// a blob id for a regular file or a symlink alike, and a directory is a 404.
func (f *forgeHTTPAPI) BlobAt(ctx context.Context, commit, p string) (string, string, error) {
	if f.kind == ForgeKindGitLab {
		u := f.repoURL("repository", "files")
		u.Path = path.Join(u.Path, p)
		u.RawPath = path.Join(u.RawPath, url.PathEscape(p))
		u.RawQuery = url.Values{"ref": {commit}}.Encode()
		var file struct {
			BlobID string `json:"blob_id"`
		}
		if err := f.get(ctx, u, &file); err != nil {
			return "", "", err
		}
		return file.BlobID, ForgeEntryFile, nil
	}
	u := f.repoURL(append([]string{"contents"}, strings.Split(p, "/")...)...)
	u.RawQuery = url.Values{"ref": {commit}}.Encode()
	var raw json.RawMessage
	if err := f.get(ctx, u, &raw); err != nil {
		return "", "", err
	}
	if len(raw) > 0 && raw[0] == '[' {
		return "", "dir", nil
	}
	var entry struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return "", "", ErrForgeUnavailable
	}
	return entry.SHA, entry.Type, nil
}

// ForgeAPIFor is the verifier's ForgeAPI hook over the automatic clones
// (F4): a client for the scope's forge with the credential the project may
// use, only when APIEligible.
func (a *AutoClones) ForgeAPIFor(ctx context.Context, projectID, scope string) (ForgeAPIClient, bool) {
	if !a.APIEligible(ctx, projectID, scope) {
		return nil, false
	}
	t, reason := a.resolve(scope)
	if reason != "" {
		return nil, false
	}
	_, header := a.credentialFor(ctx, projectID, t)
	c, ok := newForgeHTTPAPI(t, header)
	if !ok {
		return nil, false
	}
	return c, true
}
