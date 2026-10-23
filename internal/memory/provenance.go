package memory

// Provenance verification (design 22, "Amendment 2026-10-21 (#482): a
// companion-delegated chunk is verified only by provenance").
//
// A chunk an agent role wrote in a companion-delegated task is never
// role-of-record: the companion chose the workflow, so the role's authority
// is the companion's, and a companion deposit never certifies itself (#435).
// The one way such a chunk becomes verified is this check: its stored bytes
// are, byte for byte, the blob at an enumerated path in the commit
// the origin tracking ref of the project's default branch points at, in the
// project's own clone (origin/main in this repository). The check
// proves WHERE the bytes are, not who wrote them; its trust root is "merged
// to main" (see the design for who can do that and when signatures become
// necessary).
//
// Everything here reads git's OBJECT database: the working tree, where an
// uncommitted or untracked file could sit, is never read; a tree lookup never
// follows a symlink; and the only refs ever resolved are
// refs/remotes/origin/HEAD and refs/remotes/origin/<default branch>. The
// validator label ValidatorRoleProvenance keeps its "origin/main" spelling
// whatever the branch: it is a label, not the ref.

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git's object id, not a security primitive on its own; see gitBlobID
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/memoryscope"
	"vornik.io/vornik/internal/spawn"
)

// ValidatorRoleProvenance is the validator_role a provenance-verified chunk
// carries. It is a label, not evidence: the evidence is the provenance
// record, which only this verifier writes. It keeps the "origin/main"
// spelling whatever the project's default branch is.
const ValidatorRoleProvenance = "provenance:origin/main"

// ValidatorRoleProvenanceUnavailable is the validator_role of a
// companion-delegated document whose provenance could not be examined: no
// usable clone state exists for its repository (design 22, "Amendment
// 2026-10-22 (#506)", C4). Such a chunk is stored verified, as it was before
// #482, but it is not provenance: the supersession guard never lets it
// retire a ValidatorRoleProvenance chunk (C5).
const ValidatorRoleProvenanceUnavailable = "provenance:unavailable"

// The only refs the verifier reads are refs/remotes/origin/HEAD (to find the
// default branch) and refs/remotes/origin/<that branch> (see trustedBranch).

// Provenance record states.
const (
	ProvenanceVerified   = "verified"
	ProvenanceStale      = "stale"
	ProvenanceUnverified = "unverified"
)

// Provenance reasons: why a record is not verified. "" on a verified one.
const (
	ProvReasonPathNotAllowed     = "path_not_allowed"
	ProvReasonNoScope            = "no_scope"
	ProvReasonNoClone            = "no_clone"
	ProvReasonUnsafeClone        = "unsafe_clone"
	ProvReasonNoOrigin           = "no_origin"
	ProvReasonRepoMismatch       = "repo_mismatch"
	ProvReasonNoOriginMain       = "no_origin_main"
	ProvReasonNotFound           = "not_found"
	ProvReasonNotRegularFile     = "not_regular_file"
	ProvReasonBlobMismatch       = "blob_mismatch"
	ProvReasonBlobChanged        = "blob_changed"
	ProvReasonCommitUnreachable  = "commit_unreachable"
	ProvReasonContentUnavailable = "content_unavailable"
	ProvReasonGitError           = "git_error"
	// ProvReasonPendingStamp is a verification whose chunks are being
	// stamped: written before the stamp, replaced by "verified" after it.
	// The sweep re-verifies it like any transient failure.
	ProvReasonPendingStamp = "pending_stamp"
	// ProvReasonCloneFailed is a scope whose daemon-only clone could not be
	// created or was refused (#506 C1): too large, a host off the list, or
	// no access.
	ProvReasonCloneFailed = "clone_failed"
	// ProvReasonScopeNotCloneable is a scope that cannot name a repository
	// at all: not <host>/<owner>/<repo> of plain segments (security review
	// 2026-10-23). It fails closed, as an examination that failed; an
	// unlisted but well-formed host (a self-hosted forge) is clone_failed.
	ProvReasonScopeNotCloneable = "scope_not_cloneable"
)

// provenanceUnexaminable are the reasons that mean no clone exists for the
// scope at all, so the version could not be examined (design 22, "Amendment
// 2026-10-22 (#506)", C4). Only genuine absence qualifies (security review
// 2026-10-23): a clone that exists but is unsafe (a tampering signal), names
// another repository, has no origin or no trusted branch is misconfigured or
// bent, and fails closed as an examination that failed, like every git
// error (review 6f9c F14). No caller-supplied value can choose this set.
var provenanceUnexaminable = map[string]bool{
	ProvReasonNoClone:     true,
	ProvReasonCloneFailed: true,
}

// ProvenanceUnexaminable reports whether a reason means the version could not
// be examined. Such a companion document is stored as it was before #482:
// verified, validator_role ValidatorRoleProvenanceUnavailable.
func ProvenanceUnexaminable(reason string) bool { return provenanceUnexaminable[reason] }

// provenanceTerminal are the reasons a sweep never re-tries: the record can
// only be verified again by a new ingest of the document.
var provenanceTerminal = map[string]bool{
	ProvReasonPathNotAllowed:    true,
	ProvReasonNoScope:           true,
	ProvReasonCommitUnreachable: true,
}

// ProvenanceTerminal reports whether a record with this state and reason is
// left alone by the sweep.
func ProvenanceTerminal(state, reason string) bool {
	return state == ProvenanceUnverified && provenanceTerminal[reason]
}

// ProvenanceLLDDir and ProvenanceTenetsFile name the design record the
// backfill's bare-name mapping searches (ProvenanceEligibleFiles). Since #506
// C6 they no longer bound what may verify: any canonical path may.
//
// The paths are assembled from provenanceDocsDir rather than written whole
// because the public CE export (scripts/export-public-ce.sh, step 3) rewrites
// every https://docs.vornik.io and https://docs.vornik.io literal to the public docs
// site. These name files in the operator's own repository, not Vornik's
// internal docs, and a rewritten literal left CE unable to verify any path
// (gate stamp on 72001e98a, ceexport lane). Tests use these constants too.
const (
	provenanceDocsDir    = "docs"
	ProvenanceLLDDir     = provenanceDocsDir + "/low-level-design"
	ProvenanceTenetsFile = provenanceDocsDir + "/ENGINEERING-TENETS.md"
)

// ProvenanceEligibleFiles lists the single files the backfill maps a bare
// document name onto, beside the design directory.
func ProvenanceEligibleFiles() []string {
	return []string{"CLAUDE.md", ProvenanceTenetsFile}
}

// IsProvenanceEligiblePath reports whether p may be verified by provenance:
// any canonical repository path (#506 C6; design 22, "Amendment 2026-10-22
// (#506)"). The fixed list of design 22's decision 4 is retired: authority is
// the file being a regular file on the trusted branch with these exact
// bytes, which the rule's later steps check, not the directory it is in. So
// "provenance-verified" means "on the trusted branch", not "is a design".
func IsProvenanceEligiblePath(p string) bool {
	return canonicalRepoPath(p)
}

// canonicalRepoPath: valid UTF-8, no NUL/control byte, no backslash, not
// absolute, already clean, and no "." or ".." component.
func canonicalRepoPath(p string) bool {
	if p == "" || len(p) > 4096 || !utf8.ValidString(p) || strings.HasPrefix(p, "/") ||
		strings.ContainsRune(p, '\\') || path.Clean(p) != p {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// gitBlobID is git's object id of a blob with these bytes: the hash of
// "blob <len>\x00" + content, SHA-256 for a SHA-256 repository, else SHA-1.
// git computes SHA-1 with collision detection (SHA-1DC), so a known
// collision attack fails at the object store.
func gitBlobID(content []byte, sha256Repo bool) string {
	header := "blob " + strconv.Itoa(len(content)) + "\x00"
	if sha256Repo {
		h := sha256.New()
		h.Write([]byte(header))
		h.Write(content)
		return hex.EncodeToString(h.Sum(nil))
	}
	h := sha1.New() //nolint:gosec // git object id
	h.Write([]byte(header))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// ProvenanceRequest is one document version to verify.
type ProvenanceRequest struct {
	ProjectID string
	RepoScope string
	Path      string
	Content   []byte
}

// ProvenanceResult is what the verifier saw. Every field it could establish
// is filled whatever the outcome, so the record explains itself.
type ProvenanceResult struct {
	State          string
	Reason         string
	Repo           string
	Path           string
	CommitSHA      string
	ContentBlobSHA string
	OriginBlobSHA  string
	CheckedAt      time.Time
}

// Verified reports whether the result verifies the chunk.
func (r ProvenanceResult) Verified() bool { return r.State == ProvenanceVerified }

// ProvenancePrior is a stored record, as the sweep re-checks it.
type ProvenancePrior struct {
	ProjectID      string
	RepoScope      string
	Path           string
	State          string
	Reason         string
	CommitSHA      string
	ContentBlobSHA string
}

// GitRunner runs `git -C dir args...` and returns stdout. The production
// runner goes through the process-spawn law (spawn.GitWorkspace).
type GitRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// SpawnGitRunner is the production GitRunner.
func SpawnGitRunner(ctx context.Context, dir string, args ...string) ([]byte, error) {
	run := spawn.GitWorkspace
	if spawn.UnderProvenanceRoot(dir) {
		// A daemon-only provenance clone runs under its own kind, never as
		// a workspace (#506 C1, review 28a5 F2).
		run = spawn.GitProvenance
	}
	cmd, err := run(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.SetStderr(&stderr)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ProvenanceChecker is what the pipeline needs of a verifier.
type ProvenanceChecker interface {
	Verify(ctx context.Context, req ProvenanceRequest) ProvenanceResult
}

var _ ProvenanceChecker = (*ProvenanceVerifier)(nil)

// ProvenanceVerifier checks document versions against a project's clone.
type ProvenanceVerifier struct {
	// CloneDir returns the project's own clone: its workspace directory.
	CloneDir func(projectID string) string
	// CloneFor, when set, resolves the clone of one repository scope for a
	// project, replacing CloneDir in the rule (#506 C1): the configured
	// clone of the project's own repository, else the scope's automatic
	// clone, created on first use. reason is "" for a usable clone, else a
	// provenance reason (no_clone, clone_failed, unsafe_clone).
	CloneFor func(ctx context.Context, projectID, scope string) (dir, reason string)
	// DefaultBranchFor, when set, replaces DefaultBranch with a per-scope
	// answer: a project's configured branch applies only to its own
	// repository, never to another scope's clone (#506).
	DefaultBranchFor func(projectID, scope string) string
	// DefaultBranch returns the project's configured default branch
	// (workspace.refresh_branch), or "" when none is configured. Nil = none.
	DefaultBranch func(projectID string) string
	Git           GitRunner
	Now           func() time.Time
}

// NewProvenanceVerifier verifies against workspaceRoot/<projectID>.
// workspaceRoot must be registered with the spawn law
// (spawn.RegisterWorkspaceRoot) for the production runner to accept it.
func NewProvenanceVerifier(workspaceRoot string) *ProvenanceVerifier {
	return &ProvenanceVerifier{
		CloneDir: func(projectID string) string {
			if workspaceRoot == "" || projectID == "" || strings.ContainsAny(projectID, `/\`) ||
				projectID == "." || projectID == ".." {
				return ""
			}
			return filepath.Join(workspaceRoot, projectID)
		},
		Git: SpawnGitRunner,
		Now: time.Now,
	}
}

// Verify is the ingest-time check of one document version.
func (v *ProvenanceVerifier) Verify(ctx context.Context, req ProvenanceRequest) ProvenanceResult {
	res := ProvenanceResult{Path: req.Path, State: ProvenanceUnverified, CheckedAt: v.now()}
	if req.Content == nil {
		res.Reason = ProvReasonContentUnavailable
		return res
	}
	return v.evaluate(ctx, req.ProjectID, req.RepoScope, req.Path, func(sha256Repo bool) string {
		return gitBlobID(req.Content, sha256Repo)
	}, ProvenancePrior{}, res)
}

// Recheck is the sweep's re-run of the rule for a stored record. It needs no
// artifact bytes: the record holds the stored bytes' blob id.
func (v *ProvenanceVerifier) Recheck(ctx context.Context, prior ProvenancePrior) ProvenanceResult {
	res := ProvenanceResult{Path: prior.Path, State: prior.State, Reason: prior.Reason,
		CommitSHA: prior.CommitSHA, ContentBlobSHA: prior.ContentBlobSHA, CheckedAt: v.now()}
	if ProvenanceTerminal(prior.State, prior.Reason) {
		return res
	}
	res.State = ProvenanceUnverified
	if prior.ContentBlobSHA == "" {
		res.Reason = ProvReasonContentUnavailable
		return res
	}
	return v.evaluate(ctx, prior.ProjectID, prior.RepoScope, prior.Path, func(bool) string {
		return prior.ContentBlobSHA
	}, prior, res)
}

func (v *ProvenanceVerifier) now() time.Time {
	if v != nil && v.Now != nil {
		return v.Now().UTC()
	}
	return time.Now().UTC()
}

// evaluate applies the rule in order and stops at the first failure.
// contentBlob returns the stored bytes' blob id for the repository's object
// format. A prior verified record keeps its commit while it stays reachable.
func (v *ProvenanceVerifier) evaluate(ctx context.Context, projectID, scope, p string,
	contentBlob func(sha256Repo bool) string, prior ProvenancePrior, res ProvenanceResult) ProvenanceResult {
	fail := func(reason string) ProvenanceResult {
		res.State = ProvenanceUnverified
		res.Reason = reason
		return res
	}
	// 1. The path is on the list.
	if !IsProvenanceEligiblePath(p) {
		return fail(ProvReasonPathNotAllowed)
	}
	// 2. The clone exists, is safe to read, and is this repository.
	dir, repo, reason := v.checkClone(ctx, projectID, scope)
	res.Repo = repo
	if reason != "" {
		return fail(reason)
	}
	// 3. The project's default branch on origin resolves.
	branch, ok := v.trustedBranch(ctx, dir, projectID, scope)
	if !ok {
		return fail(ProvReasonNoOriginMain)
	}
	out, err := v.Git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch+"^{commit}")
	head := strings.TrimSpace(string(out))
	if err != nil || !isHexID(head) {
		return fail(ProvReasonNoOriginMain)
	}
	sha256Repo := len(head) == 64
	res.ContentBlobSHA = contentBlob(sha256Repo)
	// A verified record must still be reachable from origin/main.
	commit := head
	if prior.State == ProvenanceVerified && prior.CommitSHA != "" {
		reachable, gerr := v.reachable(ctx, dir, prior.CommitSHA, head)
		if gerr != nil {
			return fail(ProvReasonGitError)
		}
		if !reachable {
			res.CommitSHA = prior.CommitSHA
			return fail(ProvReasonCommitUnreachable)
		}
		commit = prior.CommitSHA
	}
	// 4. The path is a regular file in origin/main's tree.
	blob, reason, gerr := v.lookupBlob(ctx, dir, head, p)
	if gerr != nil {
		return fail(ProvReasonGitError)
	}
	res.OriginBlobSHA = blob
	if reason != "" {
		if prior.State == ProvenanceVerified || prior.State == ProvenanceStale {
			res.State, res.Reason = ProvenanceStale, ProvReasonBlobChanged
			res.CommitSHA = prior.CommitSHA
			return res
		}
		return fail(reason)
	}
	// 5. The stored bytes are that blob.
	if blob != res.ContentBlobSHA {
		if prior.State == ProvenanceVerified || prior.State == ProvenanceStale {
			res.State, res.Reason = ProvenanceStale, ProvReasonBlobChanged
			res.CommitSHA = prior.CommitSHA
			return res
		}
		return fail(ProvReasonBlobMismatch)
	}
	res.State, res.Reason, res.CommitSHA = ProvenanceVerified, "", commit
	return res
}

// checkClone is rule step 2: the project's clone exists, is unbent (a real
// .git directory, no grafts, alternates, shallow history or replace refs),
// and its origin is the chunk's repo scope. Returns the clone directory, the
// normalised origin (when read) and a reason, "" when every check holds.
func (v *ProvenanceVerifier) checkClone(ctx context.Context, projectID, scope string) (string, string, string) {
	scope = strings.TrimSpace(scope)
	if scope == "" || scope == "*" {
		return "", "", ProvReasonNoScope
	}
	if v == nil || v.Git == nil || (v.CloneDir == nil && v.CloneFor == nil) {
		return "", "", ProvReasonNoClone
	}
	var dir string
	if v.CloneFor != nil {
		d, reason := v.CloneFor(ctx, projectID, scope)
		if reason != "" {
			return "", "", reason
		}
		dir = d
	} else {
		dir = v.CloneDir(projectID)
	}
	if dir == "" {
		return "", "", ProvReasonNoClone
	}
	if reason := cloneUnsafe(dir); reason != "" {
		return dir, "", reason
	}
	url, err := v.Git(ctx, dir, "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(string(url)) == "" {
		return dir, "", ProvReasonNoOrigin
	}
	repo := memoryscope.NormalizeRemote(strings.TrimSpace(string(url)))
	if repo != scope {
		return dir, repo, ProvReasonRepoMismatch
	}
	// Replace refs would let a pushed ref substitute any object we read.
	out, err := v.Git(ctx, dir, "rev-parse", "--glob=refs/replace/*")
	if err != nil {
		return dir, repo, ProvReasonGitError
	}
	if strings.TrimSpace(string(out)) != "" {
		return dir, repo, ProvReasonUnsafeClone
	}
	return dir, repo, ""
}

// TrustedBranch is the branch whose origin tracking ref is the trust root
// for a project, or ok=false when it cannot be resolved (see trustedBranch).
// The operator backfill fetches exactly this branch.
func (v *ProvenanceVerifier) TrustedBranch(ctx context.Context, projectID string) (string, bool) {
	if v == nil || v.CloneDir == nil || v.Git == nil {
		return "", false
	}
	dir := v.CloneDir(projectID)
	if dir == "" {
		return "", false
	}
	return v.trustedBranch(ctx, dir, projectID, "")
}

// trustedBranch resolves the project's default branch (GitHub #482,
// security finding 2026-10-22: a hard-coded main let an unmerged branch
// named main become the root in a repository whose default is another
// branch). The configured branch (workspace.refresh_branch) when set, which
// must agree with the clone's refs/remotes/origin/HEAD when that exists;
// else origin/HEAD alone. Unresolvable, disagreeing, or not a plain branch
// name: ok=false. origin/HEAD is written by clone or `remote set-head`; the
// push guard refuses every refs/remotes/* update, so a pushing key cannot
// move it.
func (v *ProvenanceVerifier) trustedBranch(ctx context.Context, dir, projectID, scope string) (string, bool) {
	configured := ""
	switch {
	case v.DefaultBranchFor != nil:
		configured = strings.TrimSpace(v.DefaultBranchFor(projectID, scope))
	case v.DefaultBranch != nil:
		configured = strings.TrimSpace(v.DefaultBranch(projectID))
	}
	head := ""
	if out, err := v.Git(ctx, dir, "rev-parse", "--symbolic-full-name", "refs/remotes/origin/HEAD"); err == nil {
		if b, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "refs/remotes/origin/"); ok && b != "HEAD" {
			head = b
		}
	}
	if configured != "" {
		if !plainBranchName(configured) || (head != "" && head != configured) {
			return "", false
		}
		return configured, true
	}
	if head == "" || !plainBranchName(head) {
		return "", false
	}
	return head, true
}

// plainBranchName accepts a branch name that cannot read as an option, a
// revision expression or another ref namespace.
func plainBranchName(b string) bool {
	if b == "" || len(b) > 255 || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".lock") || strings.HasSuffix(b, ".") ||
		strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "@{") || b == "HEAD" {
		return false
	}
	for _, r := range b {
		if r <= 0x20 || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r) {
			return false
		}
	}
	return true
}

// cloneUnsafe refuses a clone whose object view another file could bend: a
// .git that is not a real directory, grafts, alternates, or a shallow
// history (on which reachability cannot be proven).
func cloneUnsafe(dir string) string {
	fi, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil {
		return ProvReasonNoClone
	}
	if !fi.IsDir() {
		return ProvReasonUnsafeClone
	}
	for _, f := range []string{"info/grafts", "objects/info/alternates", "objects/info/http-alternates", "shallow"} {
		if _, err := os.Lstat(filepath.Join(dir, ".git", f)); err == nil {
			return ProvReasonUnsafeClone
		}
	}
	return ""
}

// reachable reports whether commit is an ancestor of (or equal to) head:
// `rev-list --count commit ^head` is 0 exactly then. A missing object is
// unreachable, not an error.
func (v *ProvenanceVerifier) reachable(ctx context.Context, dir, commit, head string) (bool, error) {
	if !isHexID(commit) {
		return false, nil
	}
	if _, err := v.Git(ctx, dir, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return false, nil
	}
	out, err := v.Git(ctx, dir, "rev-list", "--count", commit, "^"+head)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "0", nil
}

// lookupBlob walks commit's tree one component at a time with
// `cat-file tree` (the raw binary format, parsed here), requiring every
// intermediate entry to be a tree and the leaf a regular blob. Returns the
// leaf's id, or a reason when the path is absent or not a regular file.
func (v *ProvenanceVerifier) lookupBlob(ctx context.Context, dir, commit, p string) (string, string, error) {
	raw, err := v.Git(ctx, dir, "cat-file", "commit", commit)
	if err != nil {
		return "", "", err
	}
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	tree, ok := strings.CutPrefix(string(line), "tree ")
	if !ok || !isHexID(tree) {
		return "", "", errors.New("commit object has no tree line")
	}
	segs := strings.Split(p, "/")
	id := tree
	for i, seg := range segs {
		data, err := v.Git(ctx, dir, "cat-file", "tree", id)
		if err != nil {
			return "", "", err
		}
		entries, err := parseTree(data, len(tree)/2)
		if err != nil {
			return "", "", err
		}
		e, found := entries[seg]
		if !found {
			return "", ProvReasonNotFound, nil
		}
		last := i == len(segs)-1
		switch {
		case !last && e.mode == "40000":
			id = e.id
		case last && (e.mode == "100644" || e.mode == "100755"):
			return e.id, "", nil
		case !last:
			// A blob, a symlink or a submodule where a directory must be.
			return "", ProvReasonNotRegularFile, nil
		default:
			return "", ProvReasonNotRegularFile, nil
		}
	}
	return "", ProvReasonNotFound, nil
}

type treeEntry struct {
	mode string
	id   string
}

// parseTree parses git's binary tree format: repeated
// "<mode> <name>\x00<raw id of idLen bytes>". A duplicate name is malformed.
func parseTree(data []byte, idLen int) (map[string]treeEntry, error) {
	out := map[string]treeEntry{}
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp <= 0 {
			return nil, errors.New("malformed tree entry")
		}
		mode := string(data[:sp])
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul <= 0 || len(data) < nul+1+idLen {
			return nil, errors.New("malformed tree entry")
		}
		name := string(data[:nul])
		id := hex.EncodeToString(data[nul+1 : nul+1+idLen])
		data = data[nul+1+idLen:]
		if _, dup := out[name]; dup {
			return nil, errors.New("duplicate tree entry")
		}
		out[name] = treeEntry{mode: mode, id: id}
	}
	return out, nil
}

func isHexID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ProvenanceLabel is what a recall hit says about a chunk's provenance (#506
// C7; design 22, "Amendment 2026-10-22 (#506)"): "verified" (its bytes are on
// the trusted branch), "unavailable: …" (no clone of its repository, so it is
// trusted as before #482), "unverified: …" (examined and refused, or a draft
// awaiting its push), or "" when provenance does not apply (not a
// companion-delegated document).
func ProvenanceLabel(validationStatus, validatorRole, recordState, recordReason string) string {
	switch {
	case validatorRole == ValidatorRoleProvenance && validationStatus == "verified":
		return "verified"
	case validatorRole == ValidatorRoleProvenanceUnavailable && validationStatus == "verified":
		return "unavailable: no clone of this repository could be made; trusted as before provenance checks"
	case recordState == "":
		return ""
	case recordReason == ProvReasonNotFound || recordReason == ProvReasonBlobMismatch:
		return "unverified: not yet on the trusted branch (a draft awaiting its push); re-checked automatically"
	case recordState == ProvenanceStale:
		return "unverified: the file has changed on the trusted branch since this version"
	case recordState == ProvenanceVerified:
		// The record verified but the chunk does not carry the stamp: a
		// person or an event demoted it since (security review 2026-10-23).
		// The chunk's own stamp is the only thing that says "verified".
		return "unverified: demoted after it was verified"
	default:
		return "unverified: " + recordReason
	}
}

// ChunkProvenanceLabels labels recall hits by chunk id (#506 C7), joining
// each chunk with its artifact's provenance record. Chunks with no label are
// absent from the map.
func (r *Repository) ChunkProvenanceLabels(ctx context.Context, projectID string, chunkIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if r == nil || r.db == nil || len(chunkIDs) == 0 {
		return out, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.id, c.validation_status, COALESCE(c.validator_role, ''),
		       COALESCE(p.state, ''), COALESCE(p.reason, '')
		  FROM project_memory_chunks c
		  LEFT JOIN memory_chunk_provenance p
		         ON p.project_id = c.project_id AND p.artifact_id = c.artifact_id
		 WHERE c.project_id = $1 AND c.id = ANY($2::text[])`, projectID, pq.Array(chunkIDs))
	if err != nil {
		return nil, fmt.Errorf("chunk provenance labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, status, role, state, reason string
		if err := rows.Scan(&id, &status, &role, &state, &reason); err != nil {
			return nil, err
		}
		if l := ProvenanceLabel(status, role, state, reason); l != "" {
			out[id] = l
		}
	}
	return out, rows.Err()
}
