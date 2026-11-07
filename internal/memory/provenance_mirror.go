package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/docsmeta"
	"vornik.io/vornik/internal/persistence"
)

// The repository mirror (GitHub #639; design 22, "Amendment 2026-11-04
// (#639)"). A project lists repository prefixes in memory.mirror_paths; on
// each sweep, after the trusted branch is fetched, every Markdown file under
// them whose version the project has not ingested is ingested from the
// daemon-only clone, provenance-only, so it verifies like a companion
// re-ingest. The sweep's re-checks already withdraw a changed or deleted
// version, so the mirror only brings new versions in.

// MirrorProducerRole is the producer role of a mirrored chunk.
const MirrorProducerRole = "repo-mirror"

// MirrorArtifactPrefix starts every mirrored version's artifact id.
const MirrorArtifactPrefix = "repomirror"

// mirrorMaxFilesPerPass bounds the ingests of one project in one pass; it
// counts ingests, not files read (review 385f F1).
const mirrorMaxFilesPerPass = 200

// mirrorMaxFileBytes is the companion ingest's default file cap, a constant
// so a skip keyed by blob cannot go stale (review 87b2 N4).
const mirrorMaxFileBytes = 512 << 10

// Mirror outcomes, the label of vornik_memory_mirror_files_total.
const (
	MirrorIngested     = "ingested"
	MirrorUnchanged    = "unchanged"
	MirrorQuarantined  = "quarantined"
	MirrorTerminal     = "terminal"
	MirrorNotCanonical = "not_canonical"
	MirrorNotMarkdown  = "not_markdown"
	MirrorNotRegular   = "not_regular"
	MirrorNeverMarked  = "never_marked"
	// MirrorBadFrontMatter: the front matter does not parse, so the mark
	// cannot be read; the file is skipped (fail closed).
	MirrorBadFrontMatter = "bad_front_matter"
	// MirrorHasExclusions: the file names disclosureExclude sections;
	// mirrored whole it would serve them, and stripped it could never verify.
	MirrorHasExclusions = "has_exclusions"
	MirrorTooLarge      = "too_large"
	MirrorCapped        = "capped"
	MirrorFailed        = "failed"
	MirrorNoClone       = "no_clone"
)

// MirrorProject is one project's mirror setting.
type MirrorProject struct {
	ProjectID string
	RepoScope string
	Paths     []string
}

// MirrorFile is one version the mirror ingests.
type MirrorFile struct {
	ProjectID  string
	RepoScope  string
	Path       string
	Blob       string
	ArtifactID string
	Content    []byte
}

// MirrorHooks are the mirror's ties to the store and the pipeline, wired by
// the service.
type MirrorHooks struct {
	// Projects lists the projects that set memory.mirror_paths.
	Projects func() []MirrorProject
	// Provenance returns the record for (project, artifact), or nil.
	Provenance func(ctx context.Context, projectID, artifactID string) (*persistence.ChunkProvenance, error)
	// Quarantined reports a pending or dropped quarantine row for the
	// artifact (MemoryQuarantineRepository.ExistsForArtifact).
	Quarantined func(ctx context.Context, projectID, artifactID string) (bool, error)
	// Ingest creates the artifact row when absent, ingests the version
	// provenance-only, and supersedes the path's older versions when it
	// verified.
	Ingest func(ctx context.Context, f MirrorFile) error
}

// MirrorResult is one project's pass, by outcome.
type MirrorResult struct {
	ProjectID string
	Counts    map[string]int
	// Pending is how many versions the cap left for the next pass.
	Pending int
	// At is when the pass ran.
	At time.Time
}

// mirrorLastState keeps each project's last pass, for the doctor.
type mirrorLastState struct {
	mu   sync.Mutex
	last map[string]MirrorResult
}

// LastMirrors returns each mirroring project's last pass since start.
func (s *ProvenanceSweeper) LastMirrors() []MirrorResult {
	if s == nil {
		return nil
	}
	s.mirrorLast.mu.Lock()
	defer s.mirrorLast.mu.Unlock()
	out := make([]MirrorResult, 0, len(s.mirrorLast.last))
	for _, r := range s.mirrorLast.last {
		// A copy of the counts, so no reader ever shares a map with the
		// sweep (review 3cdc F4).
		counts := make(map[string]int, len(r.Counts))
		for k, v := range r.Counts {
			counts[k] = v
		}
		r.Counts = counts
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out
}

// mirrorSkipState remembers, per (project, path), the one blob last read and
// skipped after its read (never_marked, too_large), so it is not read again
// (review 385f F1, 87b2 N2). It lives on the sweeper, for the daemon's life.
type mirrorSkipState struct {
	mu   sync.Mutex
	last map[string]string // project\x00path -> blob
}

func (s *mirrorSkipState) skipped(project, path, blob string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[project+"\x00"+path] == blob
}

func (s *mirrorSkipState) remember(project, path, blob string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]string{}
	}
	s.last[project+"\x00"+path] = blob
}

// MirrorArtifactID derives a version's artifact id from its project, scope,
// path and blob, so one version has one id and a retry reuses its row.
func MirrorArtifactID(projectID, scope, path, blob string) string {
	h := sha256.Sum256([]byte(projectID + "\x00" + scope + "\x00" + path + "\x00" + blob))
	return MirrorArtifactPrefix + "_" + hex.EncodeToString(h[:16])
}

// MirrorAll runs one mirror pass for every mirroring project. One project's
// failure is logged and does not stop the others.
func (s *ProvenanceSweeper) MirrorAll(ctx context.Context) []MirrorResult {
	if s == nil || s.Verifier == nil || s.Mirror.Projects == nil || s.Mirror.Ingest == nil ||
		s.Mirror.Provenance == nil || s.Mirror.Quarantined == nil {
		return nil
	}
	var out []MirrorResult
	for _, mp := range s.Mirror.Projects() {
		if ctx.Err() != nil {
			break
		}
		res := s.mirrorProject(ctx, mp)
		res.At = s.Verifier.now()
		out = append(out, res)
		s.mirrorLast.mu.Lock()
		if s.mirrorLast.last == nil {
			s.mirrorLast.last = map[string]MirrorResult{}
		}
		s.mirrorLast.last[mp.ProjectID] = res
		s.mirrorLast.mu.Unlock()
		if s.Metrics != nil && s.Metrics.MirrorFilesTotal != nil {
			for outcome, n := range res.Counts {
				s.Metrics.MirrorFilesTotal.WithLabelValues(mp.ProjectID, outcome).Add(float64(n))
			}
		}
		ev := s.Logger.Debug()
		if res.Counts[MirrorIngested] > 0 || res.Counts[MirrorFailed] > 0 {
			ev = s.Logger.Info()
		}
		dict := ev.Str("project_id", mp.ProjectID).Int("pending", res.Pending)
		for outcome, n := range res.Counts {
			dict = dict.Int(outcome, n)
		}
		dict.Msg("provenance sweep: repository mirror")
	}
	return out
}

type mirrorEntry struct{ path, blob string }

func (s *ProvenanceSweeper) mirrorProject(ctx context.Context, mp MirrorProject) MirrorResult {
	res := MirrorResult{ProjectID: mp.ProjectID, Counts: map[string]int{}}
	dir, head, ok := s.mirrorHead(ctx, mp)
	if !ok {
		res.Counts[MirrorNoClone]++
		return res
	}
	var entries []mirrorEntry
	for _, prefix := range mp.Paths {
		s.mirrorWalk(ctx, dir, head, strings.TrimSuffix(prefix, "/"), &entries, res.Counts)
	}
	// Nested prefixes list a file twice; keep it once (review 3cdc F6).
	seen := make(map[string]bool, len(entries))
	uniq := entries[:0]
	for _, e := range entries {
		if !seen[e.path] {
			seen[e.path] = true
			uniq = append(uniq, e)
		}
	}
	entries = uniq
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	ingested := 0
	for i, e := range entries {
		if ctx.Err() != nil {
			break
		}
		if ingested >= mirrorMaxFilesPerPass {
			res.Pending = len(entries) - i
			res.Counts[MirrorCapped] += res.Pending
			break
		}
		outcome := s.mirrorOne(ctx, mp, dir, e)
		res.Counts[outcome]++
		if outcome == MirrorIngested {
			ingested++
		}
	}
	return res
}

// mirrorHead resolves the clone and the trusted branch's head exactly as
// verification does (checkClone: CloneFor, origin is the scope, no replace
// refs; then trustedBranch), so the mirror never reads a clone the verifier
// would refuse. originMain is the backfill's view (CloneDir only) and is not
// used here.
func (s *ProvenanceSweeper) mirrorHead(ctx context.Context, mp MirrorProject) (string, string, bool) {
	v := s.Verifier
	dir, _, reason := v.checkClone(ctx, mp.ProjectID, mp.RepoScope)
	if reason != "" {
		return "", "", false
	}
	branch, ok := v.trustedBranch(ctx, dir, mp.ProjectID, mp.RepoScope)
	if !ok {
		return "", "", false
	}
	out, err := v.Git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch+"^{commit}")
	head := strings.TrimSpace(string(out))
	if err != nil || !isHexID(head) {
		return "", "", false
	}
	return dir, head, true
}

// mirrorWalk lists the regular Markdown files under prefix on the trusted
// branch with the verifier's own tree reader (lookupTree, cat-file tree,
// parseTree), recursing into subtrees.
func (s *ProvenanceSweeper) mirrorWalk(ctx context.Context, dir, head, prefix string, out *[]mirrorEntry, counts map[string]int) {
	treeID, reason, err := s.Verifier.lookupTree(ctx, dir, head, prefix)
	if err != nil || reason != "" {
		return // the prefix is not on the branch
	}
	var walk func(id, at string)
	walk = func(id, at string) {
		if ctx.Err() != nil {
			return
		}
		data, err := s.Verifier.Git(ctx, dir, "cat-file", "tree", id)
		if err != nil {
			return
		}
		tree, err := parseTree(data, len(head)/2)
		if err != nil {
			return
		}
		for name, e := range tree {
			p := at + "/" + name
			switch {
			case e.mode == "40000":
				walk(e.id, p)
			case !strings.HasSuffix(name, ".md"):
				counts[MirrorNotMarkdown]++
			case e.mode != "100644" && e.mode != "100755":
				counts[MirrorNotRegular]++
			default:
				*out = append(*out, mirrorEntry{path: p, blob: e.id})
			}
		}
	}
	walk(treeID, prefix)
}

// mirrorOne decides and, when due, ingests one version (M2 steps 3 and 4).
func (s *ProvenanceSweeper) mirrorOne(ctx context.Context, mp MirrorProject, dir string, e mirrorEntry) string {
	if !canonicalRepoPath(e.path) {
		return MirrorNotCanonical
	}
	if s.mirrorSkips.skipped(mp.ProjectID, e.path, e.blob) {
		return MirrorUnchanged
	}
	id := MirrorArtifactID(mp.ProjectID, mp.RepoScope, e.path, e.blob)
	rec, err := s.Mirror.Provenance(ctx, mp.ProjectID, id)
	if err != nil {
		return MirrorFailed
	}
	if rec != nil {
		if !ProvenanceTerminal(rec.State, rec.Reason) {
			return MirrorUnchanged // the sweep handles it
		}
		if rec.Reason != ProvReasonCommitUnreachable {
			return MirrorTerminal // stable: re-ingesting writes the same record
		}
	}
	if q, err := s.Mirror.Quarantined(ctx, mp.ProjectID, id); err != nil {
		return MirrorFailed
	} else if q {
		return MirrorQuarantined
	}
	if !isHexID(e.blob) {
		return MirrorFailed
	}
	content, err := s.Verifier.Git(ctx, dir, "cat-file", "blob", e.blob)
	if err != nil {
		return MirrorFailed
	}
	if len(content) > mirrorMaxFileBytes {
		s.mirrorSkips.remember(mp.ProjectID, e.path, e.blob)
		return MirrorTooLarge
	}
	// Fail closed (security review 2026-11-05): an unreadable mark, a never
	// mark, or named exclusions all keep the file out.
	mark, exclude, perr := docsmeta.ParseDisclosure(content)
	switch {
	case perr != nil:
		s.mirrorSkips.remember(mp.ProjectID, e.path, e.blob)
		return MirrorBadFrontMatter
	case mark == docsmeta.DisclosureNever:
		s.mirrorSkips.remember(mp.ProjectID, e.path, e.blob)
		return MirrorNeverMarked
	case len(exclude) > 0:
		s.mirrorSkips.remember(mp.ProjectID, e.path, e.blob)
		return MirrorHasExclusions
	}
	if err := s.Mirror.Ingest(ctx, MirrorFile{ProjectID: mp.ProjectID, RepoScope: mp.RepoScope,
		Path: e.path, Blob: e.blob, ArtifactID: id, Content: content}); err != nil {
		s.Logger.Warn().Err(err).Str("project_id", mp.ProjectID).Str("path", e.path).
			Msg("provenance sweep: mirror ingest failed; the next pass retries")
		return MirrorFailed
	}
	return MirrorIngested
}

// MirrorArtifactStore is the artifact-row half of a mirror ingest.
type MirrorArtifactStore interface {
	Get(ctx context.Context, id string) (*persistence.Artifact, error)
	Create(ctx context.Context, a *persistence.Artifact) error
}

// MirrorIngester lands one mirrored version (design 22, "Amendment
// 2026-11-04 (#639)", M2 step 4): its artifact row, read before it is
// created because the companion hook is a plain insert and a retry would
// fail on it; the provenance-only ingest through the gated pipeline; and,
// when the version verified, supersession of the path's older versions.
type MirrorIngester struct {
	Artifacts MirrorArtifactStore
	Pipeline  *Pipeline
	Supersede func(ctx context.Context, projectID, scope, path string) (int, error)
}

// Ingest implements MirrorHooks.Ingest.
func (m MirrorIngester) Ingest(ctx context.Context, f MirrorFile) error {
	size := int64(len(f.Content))
	if _, err := m.Artifacts.Get(ctx, f.ArtifactID); err != nil {
		if !errors.Is(err, persistence.ErrNotFound) {
			return err
		}
		// An upload (INPUT, origin upload), like the companion rag-ingest's
		// staged documents: document supersession retires and keeps only
		// uploads (memory.Repository.SupersedeDocument, its upload-only
		// invariant), so a mirrored version and a companion upload of the
		// same path compete for the one live version. No task: nothing
		// reads its storage path, the content is ingested from memory.
		if err := m.Artifacts.Create(ctx, &persistence.Artifact{
			ID: f.ArtifactID, ProjectID: f.ProjectID, Name: f.Path,
			ArtifactClass: persistence.ArtifactClassInput, StoragePath: MirrorArtifactPrefix + "://inline",
			SizeBytes: &size, Origin: persistence.ArtifactOriginUpload,
		}); err != nil {
			return err
		}
	}
	stats, err := m.Pipeline.IngestArtifactWithOptions(ctx, f.ProjectID, "", f.ArtifactID, f.Path,
		MirrorProducerRole, "", string(f.Content), size, "",
		IngestArtifactOptions{ClassOverride: ClassSpec, RepoScope: f.RepoScope, Document: true, ProvenanceOnly: true})
	if err != nil {
		return err
	}
	// Supersede only a version that verified, so an unverified version never
	// retires a verified one. A supersede that fails here is not lost: the
	// sweep runs Supersede for every provenance-verified record on each pass
	// (#506 C9), so the old version is retired then (review 3cdc F3).
	if stats.Verified > 0 && m.Supersede != nil {
		if _, err := m.Supersede(ctx, f.ProjectID, f.RepoScope, f.Path); err != nil {
			return err
		}
	}
	return nil
}
