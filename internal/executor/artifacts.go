package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/llmspend"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
	"vornik.io/vornik/internal/taintlineage"
)

// ArtifactDirSnapshot is a content-hash map of the files in an
// artifact-output directory, keyed by filename. Captured at step
// start by SnapshotArtifactDir and consumed by persistArtifacts to
// detect cross-task pollution: a file whose post-step hash matches
// its pre-step hash wasn't modified by this step, so registering
// it as the step's output would be a lie.
//
// Why content hash, not mtime: per-task git worktrees set every
// checked-out file's mtime to the checkout time, which sails past
// the original `mtime >= stepStart` filter. The 2026-05-21
// headphone-research incident reproduced this: T-1a83's research
// step captured T-7986's write-step deliverable (same hash) as
// its own output because the worktree-fresh mtime cleared the
// stale-file filter.
type ArtifactDirSnapshot map[string]string

// SnapshotArtifactDir walks `dir` non-recursively and returns the
// SHA-256 (hex) of every regular file. Empty / missing dirs return
// an empty (non-nil) map; errors on individual files skip that
// file and log a warning. The result is safe to pass to
// persistArtifacts even when zero files exist.
//
// Designed for the artifacts/out/ tree which is normally <20
// small markdown files. A per-file size cap (4 MiB) guards
// against an accidental large blob in that tree blowing up the
// step setup latency.
func (e *Executor) SnapshotArtifactDir(dir string) ArtifactDirSnapshot {
	const maxHashableBytes = 4 * 1024 * 1024
	out := ArtifactDirSnapshot{}
	if dir == "" {
		return out
	}
	// One root descriptor for the listing and every open (GitHub #359 H4):
	// a FIFO here used to hang step setup, and the hashes feed the
	// post-step staleness gate.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return out
	}
	defer func() { _ = root.Close() }()
	for _, entry := range listHarvestDir(root, ".") {
		if entry.IsDir() {
			continue
		}
		f, info, openErr := openHarvestFile(root, entry.Name())
		if openErr != nil {
			continue
		}
		if info.Size() > maxHashableBytes {
			// Too big to hash on the hot path. Record the size as
			// a stand-in fingerprint — a same-name file with a
			// different size still gets re-registered, which is
			// the conservative direction (false positives are
			// harmless extra artifact rows; false negatives drop
			// real outputs).
			out[entry.Name()] = fmt.Sprintf("size:%d", info.Size())
			_ = f.Close()
			continue
		}
		h, err := hashOpenFile(f)
		_ = f.Close()
		if err != nil {
			if e != nil {
				e.logger.Debug().Err(err).Str("entry", entry.Name()).
					Msg("artifacts: pre-step hash failed; will trust mtime gate")
			}
			continue
		}
		out[entry.Name()] = h
	}
	return out
}

// hashOpenFile hashes f from its start and rewinds it, so the same
// descriptor can then be handed to the store, whose StoreFile reads from
// the current offset (GitHub #359 H2). Do not drop the final rewind.
func hashOpenFile(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// openHarvestFile opens rel beneath root for the step-output harvest
// (GitHub #359, 15-artifact-store.md §13.3 H1). root is opened ONCE per
// harvest source and shared by the listing (listHarvestDir) and every open,
// so the directory listed is the directory read. os.Root resolves every
// component relative to the previous component's descriptor with
// O_NOFOLLOW, follows a symlink only when it is relative and stays beneath
// root, and refuses an absolute or escaping one; so a component swapped
// for a symlink after the listing either still lands beneath root or the
// open fails. O_NONBLOCK keeps a FIFO from blocking the open; the Fstat on
// the opened descriptor must report a regular file. Every later decision
// about the file reads this descriptor, never the path again.
func openHarvestFile(root *os.Root, rel string) (*os.File, os.FileInfo, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s: not a regular file (%s)", rel, info.Mode().Type())
	}
	return f, info, nil
}

// logHarvestRootError makes a harvest source that could not be opened
// visible (GitHub #359 review round 2): the source then yields no outputs,
// which must not look like a step that produced none. A missing directory
// is the ordinary "nothing here" case and stays quiet.
func (e *Executor) logHarvestRootError(dir string, err error) {
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}
	e.logger.Warn().Err(err).Str("root", dir).
		Msg("artifacts: harvest root could not be opened; its outputs are not harvested")
}

// listHarvestDir lists rel beneath root (GitHub #359 H1): the directory is
// opened through the same root as the files, so a directory swapped for a
// symlink out of the root lists nothing. A missing or refused directory
// lists nothing, as os.ReadDir's error did before.
func listHarvestDir(root *os.Root, rel string) []os.DirEntry {
	d, err := root.Open(rel)
	if err != nil {
		return nil
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil
	}
	return entries
}

// harvestBeforeOpenHook is a test seam (GitHub #359): it runs between a
// harvested entry's listing and the read of its bytes, so a test can swap
// the entry deterministically. nil in production.
var harvestBeforeOpenHook func(name string)

// harvestedFile is one file the executor found during a step's
// artifact scan: its on-disk name, the harvest root it was listed under
// and its path beneath that root. The bytes are read only through
// openHarvestFile(root, rel) (GitHub #359); sourcePath (root/rel) is a
// label for logs and for the repo-only fallback row, never re-opened.
type harvestedFile struct {
	name       string
	root       *os.Root
	rel        string
	sourcePath string
	// snapshotKey names this file's pre-step hash in preStepSnapshot
	// ("" = not snapshot-gated). projectTree marks source #3, which is
	// also mtime-gated.
	snapshotKey string
	projectTree bool
}

func newHarvestedFile(root *os.Root, rel, name string) harvestedFile {
	return harvestedFile{name: name, root: root, rel: rel, sourcePath: filepath.Join(root.Name(), rel)}
}

// collectProjectPersistedArtifacts lists source #3: the project-
// persisted artifacts/out/ tree under effectiveProjectDir (the worktree,
// or the project's persistent root in non-worktree fallback). This tree
// survives across per-step containers and across tasks, so it is gated
// twice (in fileIsStale, from the opened descriptor) to avoid registering
// stale files from prior tasks as this step's output:
//
//   - mtime gate: files modified before stepStart (minus 1s slack)
//     belong to a prior task. Disabled when stepStart is zero.
//   - content-hash gate: per-task git worktrees touch every checked-out
//     file's mtime, so a file merged in from a prior task carries a
//     "fresh" mtime even though its bytes are stale. A file present in
//     preStepSnapshot with identical bytes was NOT written this step.
//
// Extracted from persistArtifacts to keep that function within the
// gocognit budget. See persistArtifacts' source-#3 comment for the full
// 2026-05-18 path-correction history.
func (e *Executor) collectProjectPersistedArtifacts(projectRoot *os.Root) []harvestedFile {
	if projectRoot == nil {
		return nil
	}
	var files []harvestedFile
	for _, entry := range listHarvestDir(projectRoot, filepath.Join("artifacts", "out")) {
		if entry.IsDir() {
			continue
		}
		f := newHarvestedFile(projectRoot, filepath.Join("artifacts", "out", entry.Name()), entry.Name())
		f.snapshotKey = entry.Name()
		f.projectTree = true
		files = append(files, f)
	}
	return files
}

// fileIsStale reports whether a harvested file must NOT be registered as
// this step's output, deciding from the opened descriptor (GitHub #359 H2):
// a source-#3 file older than stepStart (mtime gate), or any snapshot-gated
// file whose bytes equal its pre-step hash (an unchanged staged input,
// T-b093f, or a file inherited unchanged via a worktree merge).
func (e *Executor) fileIsStale(f harvestedFile, info os.FileInfo, hash string, stepStart time.Time, preStepSnapshot ArtifactDirSnapshot) bool {
	if f.projectTree && !stepStart.IsZero() && info.ModTime().Before(stepStart.Add(-1*time.Second)) {
		e.logger.Debug().
			Str("entry", f.name).
			Time("mtime", info.ModTime()).
			Time("step_start", stepStart).
			Msg("artifacts: skipping project-persisted file predating step start")
		return true
	}
	if f.snapshotKey == "" || preStepSnapshot == nil {
		return false
	}
	prevHash, seen := preStepSnapshot[f.snapshotKey]
	if seen && prevHash == hash {
		e.logger.Debug().
			Str("entry", f.name).
			Str("hash", hash).
			Msg("artifacts: skipping file unchanged since step start")
		return true
	}
	return false
}

// persistArtifacts stores output files from the workspace into the artifact store.
// It collects files from three locations:
//  1. workspace/artifacts/out/ — the standard agent output artifact directory
//  2. workspace root — files created by agent tool calls (file_write) at the
//     workspace root
//  3. workspace/project/artifacts/out/ — the project-persisted artifact tree
//     that survives across per-step containers. The writer's file_write
//     deliverables (PDF/DOCX) land here when the agent is configured to use
//     the project-persisted output path; without this walk they'd never
//     become DB artifact rows and the deliverable surfacing (Telegram
//     document push, /ui/projects/<id>/artifacts list) would render empty.
//
// preStepSnapshot is the SHA-256-by-name fingerprint of source #3
// captured at step start (via SnapshotArtifactDir). Files whose
// post-step hash matches the snapshot were NOT modified by this
// step and are skipped. stepStart is still used as a cheap mtime
// fast-path but is no longer load-bearing on its own — per-task
// git worktrees touch every checked-out file's mtime at task
// start, so a stale file inherited from a previous task's merge
// sails past the mtime filter. The hash check catches what mtime
// misses. The ephemeral sources (#1, #2) are not snapshot-filtered:
// those dirs are created fresh per ephemeral container, or wiped
// on warm-container release.
//
// Directories, hidden files, and the artifacts/ subtree itself are skipped
// when scanning the workspace root to avoid duplicates.
//
// Returns the harvested outputs as {name, sourcePath} maps so the
// workflow loop can re-stage them into the next step's ephemeral
// container (task e9a5). name is the ORIGINAL on-disk filename (the
// next step's role reads by logical name, not the disambiguated store
// name); sourcePath is the durable store StoragePath when an
// artifactStore is wired (it lives under an allowed staging root so
// resolveStagingSrc admits it), or the harvested host path on the
// artifactRepo-only fallback.
func (e *Executor) persistArtifacts(ctx context.Context, executionID, projectID, taskID, workspaceDir, effectiveProjectDir string, stepStart time.Time, preStepSnapshot ArtifactDirSnapshot) ([]map[string]string, error) {
	var files []harvestedFile

	// 1. Standard artifact output directory. Entries are only LISTED here;
	// each is opened beneath workspaceDir by openHarvestFile below, so a
	// symlink planted by the agent container (e.g. artifacts/out/foo ->
	// /etc/passwd), or swapped in after this listing while a warm
	// container is still running, cannot be read (GitHub #359).
	//
	// Prior-step outputs are staged into this directory so the next role
	// can read them. If the role left one byte-for-byte unchanged, it
	// remains an input and must not be registered again as this step's
	// output (T-b093f): snapshotKey "workspace:<name>". A modified
	// same-name file is a genuine new revision and passes through.
	//
	// One root descriptor per source, opened here and shared by the listing
	// and every open, so the tree listed is the tree read (#359 H1).
	wsRoot, wsErr := os.OpenRoot(workspaceDir)
	e.logHarvestRootError(workspaceDir, wsErr)
	if wsErr == nil {
		defer func() { _ = wsRoot.Close() }()
		for _, entry := range listHarvestDir(wsRoot, filepath.Join("artifacts", "out")) {
			if entry.IsDir() {
				continue
			}
			f := newHarvestedFile(wsRoot, filepath.Join("artifacts", "out", entry.Name()), entry.Name())
			f.snapshotKey = "workspace:" + entry.Name()
			files = append(files, f)
		}

		// 2. Workspace root — agent-created files (e.g. via file_write tool).
		// Skip internal/temp files and files that result from null tool
		// arguments (jq -r on JSON null outputs the literal string "null").
		for _, entry := range listHarvestDir(wsRoot, ".") {
			name := entry.Name()
			if entry.IsDir() || strings.HasPrefix(name, ".") || name == "null" {
				continue
			}
			files = append(files, newHarvestedFile(wsRoot, name, name))
		}
	}

	// 3. Project-persisted artifact output directory. Survives across
	// per-step containers; the writer's file_write places PDF/DOCX
	// deliverables here. Without this walk they'd never become DB
	// artifact rows and the Telegram/UI deliverable surfacing would
	// render empty (see 2026-05-17 CV-PDF retry incident).
	//
	// Path correction (2026-05-18): walk the worktree (or, in non-
	// worktree fallback mode, the project's persistent workspace
	// root) directly via effectiveProjectDir. The previous path
	// `workspaceDir/project/artifacts/out/` was the container's
	// view: `/app/workspace/project/` is bind-mounted to the worktree
	// inside the container, but on the host that subdirectory is
	// always empty (the bind only exists in the container's mount
	// namespace). Walking it from the host found nothing, so every
	// PDF/DOCX/HTML the writer produced into the worktree was
	// invisible to persistArtifacts — the artifacts UI listing and
	// Telegram deliverable surfacing both come up empty. effective-
	// ProjectDir mirrors verifyClaimedFiles' source-of-truth path:
	// worktreeDir when worktrees are in use, otherwise the project's
	// persistent root.
	//
	// De-dup note: a file at BOTH ephemeral artifacts/out/X.md AND
	// project/artifacts/out/X.md gets registered twice, but the
	// existing disambiguateArtifactName mints distinct
	// `{stem}-YYYYMMDD-XXXX{ext}` names per execution so both rows
	// have distinct names + storage paths — no operator-visible
	// collision.
	//
	// Mtime gating: stepStart is the wall-clock floor. The project
	// tree persists across tasks, so without this filter every new
	// task would re-register every stale file under the tree as ITS
	// artifact. 1s slack mirrors verify.go::verifyClaimedFiles to
	// account for sub-second mtime resolution.
	if effectiveProjectDir != "" {
		projectRoot, err := os.OpenRoot(effectiveProjectDir)
		e.logHarvestRootError(effectiveProjectDir, err)
		if err == nil {
			defer func() { _ = projectRoot.Close() }()
			files = append(files, e.collectProjectPersistedArtifacts(projectRoot)...)
		}
	}

	// Disambiguate every harvested filename before persisting so
	// multiple tasks producing the same logical name (e.g. two
	// different research.md outputs) end up with distinct artifact
	// records, distinct on-disk paths, and distinct memory chunks.
	// Pre-fix: two tasks writing `research.md` produced identical
	// artifact.Name values, and memory search returned both
	// chunks under that one label — the LLM couldn't distinguish
	// them. See disambiguateArtifactName for the format details.
	//
	// System-named artifacts (CHANGES.md from plan_step, backstop
	// stubs) bypass this path because they're produced via direct
	// artifactStore.Store calls with deterministic names that
	// downstream consumers exact-match against — disambiguating
	// them would break those consumers without solving any real
	// collision (system artifacts are already 1:1 with executions
	// via their per-execution directory).
	now := time.Now().UTC()
	// harvested carries the outputs back to the workflow loop for
	// cross-step staging (task e9a5). Keyed on the ORIGINAL name so the
	// next step's role finds the file by its logical filename.
	harvested := make([]map[string]string, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		entry, err := e.harvestOne(ctx, executionID, projectID, taskID, f, now, stepStart, preStepSnapshot, seen)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			harvested = append(harvested, entry)
		}
	}
	return harvested, nil
}

// harvestOne opens one listed file beneath its root (GitHub #359 H1),
// applies the staleness gates and the same-name/same-bytes collapse from
// that descriptor (H2), and stores it from the same descriptor (H3). An
// entry the opener refuses (an escaping or absolute symlink, a FIFO or other
// non-regular file, a vanished file) is skipped with a warning, as the
// path check that preceded #359 skipped an escaping entry.
func (e *Executor) harvestOne(ctx context.Context, executionID, projectID, taskID string, f harvestedFile, now, stepStart time.Time, preStepSnapshot ArtifactDirSnapshot, seen map[string]bool) (map[string]string, error) {
	if harvestBeforeOpenHook != nil {
		harvestBeforeOpenHook(f.name)
	}
	src, info, err := openHarvestFile(f.root, f.rel)
	if err != nil {
		e.logger.Warn().Err(err).Str("entry", f.sourcePath).
			Msg("artifacts: refusing entry that is not a regular file beneath its harvest root")
		return nil, nil
	}
	defer func() { _ = src.Close() }()
	hash, err := hashOpenFile(src)
	if err != nil {
		return nil, fmt.Errorf("artifact hash for %s: %w", f.name, err)
	}
	if e.fileIsStale(f, info, hash, stepStart, preStepSnapshot) {
		return nil, nil
	}
	// Unchanged staged inputs were removed by the gate above; now collapse
	// identical same-name files from both output roots before
	// disambiguation, without crossing harvests or task boundaries.
	key := f.name + ":" + hash
	if seen[key] {
		return nil, nil
	}
	seen[key] = true
	return e.storeOneArtifact(ctx, executionID, projectID, taskID, f.name, f.sourcePath, src, now)
}

// storeOneArtifact persists a single harvested file through the
// configured backend (artifactStore preferred, artifactRepo fallback)
// and returns the {name, sourcePath} handoff entry for cross-step
// staging (task e9a5), or nil when nothing is persisted (no backend
// wired, or the store returned no path). name is the ORIGINAL on-disk
// name; sourcePath is the durable store StoragePath when an
// artifactStore is wired, else the harvested host path on the repo-only
// fallback. Split out of persistArtifacts to keep that function's
// cognitive complexity within the gocognit budget.
func (e *Executor) storeOneArtifact(ctx context.Context, executionID, projectID, taskID, name, sourcePath string, src *os.File, now time.Time) (map[string]string, error) {
	disambiguated := disambiguateArtifactName(name, executionID, now)
	if e.artifactStore != nil {
		// The store reads the descriptor the harvest opened and checked,
		// never sourcePath (GitHub #359 H3).
		stored, err := e.artifactStore.StoreFile(ctx, projectID, executionID, taskID, disambiguated, src)
		if err != nil {
			return nil, fmt.Errorf("artifact store failed for %s: %w", disambiguated, err)
		}
		// Forward the durable store path (under an allowed staging
		// root) so the next step can re-stage it.
		if stored == nil || stored.StoragePath == "" {
			return nil, nil
		}
		return map[string]string{"name": name, "sourcePath": stored.StoragePath}, nil
	}

	var harvested map[string]string
	if e.artifactRepo != nil {
		artifact := &persistence.Artifact{
			ID:              generateArtifactID(executionID),
			ProjectID:       projectID,
			ExecutionID:     &executionID,
			Name:            disambiguated,
			ArtifactClass:   persistence.ArtifactClassOutput,
			StoragePath:     sourcePath,
			Origin:          persistence.ArtifactOriginTaskOutput,
			ProducingStepID: persistence.ArtifactStep(ctx),
		}
		if taskID != "" {
			artifact.TaskID = &taskID
		}
		if err := e.artifactRepo.Create(ctx, artifact); err != nil {
			return nil, fmt.Errorf("artifact record failed for %s: %w", disambiguated, err)
		}
		// No store StoragePath on the repo-only fallback; the harvested
		// file's own host path is the source to stage from (it was just
		// persisted as the artifact row).
		harvested = map[string]string{"name": name, "sourcePath": sourcePath}
	}
	// Feature #3 — emit file_edit for the live observation surface. Op
	// is "create" for outputs (produced_files don't distinguish
	// modify/delete today). Size + hash stay empty here; the replay
	// page reads the full metadata from the artifact row when the
	// operator drills down. Preserves the pre-refactor behaviour where
	// this fired on the repo path AND the no-backend path (the store
	// path `continue`d past it).
	e.emitFileEdit(ctx, executionID, "", disambiguated, "create", "", 0)
	return harvested, nil
}

// disambiguateArtifactName rewrites a raw artifact filename to
// guarantee uniqueness across tasks and re-runs. Format:
//
//	{stem}-{YYYYMMDD}-{4hex}{ext}
//
// where YYYYMMDD is the UTC date of the produce-time and 4hex is
// the last four chars of the execution_id (mirroring the UI's
// shortID convention so operators can correlate filenames back to
// executions visually).
//
// Why both date AND short ID:
//   - YYYYMMDD makes the filename sortable chronologically — an
//     operator scrolling through a long artifact list can see at
//     a glance which day each was produced.
//   - 4 hex of execution_id gives 65k uniqueness inside a single
//     day for a single project — more than enough headroom for
//     any realistic burst, with minimal visual weight.
//
// Extension handling: the LAST dot in the name (if any, not
// counting a leading-dot like .env) marks the extension. Stem is
// everything before it. Names with no extension (CHANGELOG,
// Makefile, Dockerfile) get the suffix appended directly without
// a trailing extension.
//
// Idempotency: not enforced. If the input already looks like a
// disambig'd name, this function adds ANOTHER suffix on top —
// callers should disambiguate exactly once at the harvest
// boundary. The persistArtifacts caller is the single canonical
// invocation point; system-named artifacts (CHANGES.md, backstop
// stubs) bypass this path and so don't double-suffix.
func disambiguateArtifactName(name, executionID string, when time.Time) string {
	stem, ext := splitArtifactStem(name)
	short := executionID
	if len(executionID) > 4 {
		short = executionID[len(executionID)-4:]
	}
	return fmt.Sprintf("%s-%s-%s%s", stem, when.UTC().Format("20060102"), short, ext)
}

// splitArtifactStem separates an artifact filename into its stem
// and extension. The LAST dot wins as the extension boundary so
// `report.tar.gz` splits into stem=`report.tar`, ext=`.gz` — that
// matches operator intuition (the extension is what tells you
// what kind of file it is) and lets the disambig suffix land in
// the natural position before the final dot.
//
// Leading-dot files (`.env`, `.dockerignore`) are treated as
// extension-less. Returning ext="" leaves the disambig suffix to
// be appended directly to the stem, producing forms like
// `.env-20260516-1a2b` rather than the surprising `-20260516-1a2b.env`.
//
// Idempotent contract: stem+ext == name for any input.
func splitArtifactStem(name string) (stem, ext string) {
	dot := strings.LastIndex(name, ".")
	if dot <= 0 {
		// Either no dot (CHANGELOG) or leading dot only (.env).
		return name, ""
	}
	return name[:dot], name[dot:]
}

// disambigSuffixRe matches the trailing `-YYYYMMDD-XXXX` segment that
// disambiguateArtifactName appends to every harvested filename's
// stem. Anchored to end-of-string so a name like
// `request-20260516-cycle.md` (where the date segment is NOT at the
// end) is not mistakenly stripped — only the disambig contract's
// final placement matches.
var disambigSuffixRe = regexp.MustCompile(`-\d{8}-[0-9a-fA-F]{4}$`)

// stripDisambiguationSuffix returns the artifact's original
// (pre-disambiguation) name. Inverse of disambiguateArtifactName for
// names that match its `{stem}-YYYYMMDD-XXXX{ext}` shape; returns
// the input unchanged otherwise. Idempotent and safe to call on any
// filename, including ones produced by older vornik builds before
// disambiguation existed.
//
// Used by content-routing helpers (e.g. isTranscriptArtifact below)
// that need to match against the artifact's logical name regardless
// of when it was produced.
func stripDisambiguationSuffix(name string) string {
	stem, ext := splitArtifactStem(name)
	if m := disambigSuffixRe.FindStringIndex(stem); m != nil && m[1] == len(stem) {
		return stem[:m[0]] + ext
	}
	return name
}

// OriginalArtifactName returns the name an agent gave an output file, before
// the executor's -YYYYMMDD-xxxx disambiguation suffix. The companion broker
// egress path matches a workflow's declared egress.output against it.
func OriginalArtifactName(name string) string {
	return stripDisambiguationSuffix(name)
}

// isTranscriptArtifact reports whether the artifact name follows
// the per-step execution-transcript convention `<step>-response.md`.
// These artifacts capture an agent step's raw output for the UI's
// execution-detail view; they are NOT load-bearing project knowledge
// and must be excluded from memory ingest. Pre-2026-05-15 the
// filter was a plain `HasSuffix("-response.md")` check, but the
// 2026-05-15 disambiguation commit (6cbb9c7) appended
// `-YYYYMMDD-XXXX` to every harvested filename, leaving names like
// `route-response-20260515-0f96.md` that the old check missed.
// Result: 79 transcript chunks (8 of `route-response`, 4 of
// `write-response`, etc.) landed in project memory with empty
// producer_role and stuck unclassified until manual reclassify.
// This helper restores the original semantics by stripping
// disambiguation first.
func isTranscriptArtifact(name string) bool {
	base := stripDisambiguationSuffix(name)
	return strings.HasSuffix(base, "-response.md")
}

// IsTranscriptArtifact is the exported face of isTranscriptArtifact for
// sibling packages that present output artifacts to consumers (the
// companion MCP result() inliner, 2026-06-07). One implementation —
// memory ingest and result presentation must never drift on what counts
// as a transcript.
func IsTranscriptArtifact(name string) bool {
	return isTranscriptArtifact(name)
}

// recordLLMUsageFromResult parses the usage block from agent-container
// result.json, emits Prometheus counters, and persists a TaskLLMUsage row.
// Silent on missing fields — old agent images without the usage block
// simply don't record. Metric emission and DB persistence are independent:
// a nil metrics target skips only Prometheus; a nil repo skips only DB.
func (e *Executor) recordLLMUsageFromResult(ctx context.Context, task *persistence.Task, execution *persistence.Execution, stepID, role, model string, resultBytes []byte) {
	var parsed struct {
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			Iterations          int `json:"iterations"`
			CacheCreationTokens int `json:"cache_creation_tokens"`
			CacheReadTokens     int `json:"cache_read_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resultBytes, &parsed); err != nil {
		return
	}
	if parsed.Usage.PromptTokens == 0 && parsed.Usage.CompletionTokens == 0 && parsed.Usage.Iterations == 0 {
		return
	}

	if e.metrics != nil {
		e.metrics.RecordLLMUsageWithCache(task.ProjectID, role, model,
			parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, parsed.Usage.Iterations,
			parsed.Usage.CacheCreationTokens, parsed.Usage.CacheReadTokens, e.pricing)
	}

	{
		costUSD := 0.0
		if e.pricing != nil {
			costUSD = e.pricing.CostUSDWithCache(model,
				parsed.Usage.PromptTokens,
				parsed.Usage.CompletionTokens,
				parsed.Usage.CacheCreationTokens,
				parsed.Usage.CacheReadTokens)
		}
		taskID := task.ID
		execID := execution.ID
		// Same deterministic id the agent's per-iteration stream lands on, so the
		// finalize batch and the streaming path collide on the (id) PK and upsert
		// cleanly instead of producing two rows per step. The finalize path runs
		// last, so it wins the final write.
		//
		// Derived in llmspend rather than formatted here: the id must name the
		// EXECUTION as well as the step. Without it a retry overwrote the
		// execution it retried, erasing that attempt's spend from the ledger
		// entirely (see llmspend/stepusage.go).
		if err := e.spend.Upsert(ctx, llmspend.StepUsageID(taskID, execID, stepID, role), llmspend.Input{
			ProjectID:           task.ProjectID,
			Model:               model,
			PromptTokens:        parsed.Usage.PromptTokens,
			CompletionTokens:    parsed.Usage.CompletionTokens,
			TaskID:              &taskID,
			ExecutionID:         &execID,
			StepID:              stepID,
			RoleOverride:        role,
			CostUSD:             &costUSD,
			APIKeyID:            task.CreatedByAPIKeyID,
			Iterations:          parsed.Usage.Iterations,
			CacheCreationTokens: parsed.Usage.CacheCreationTokens,
			CacheReadTokens:     parsed.Usage.CacheReadTokens,
		}); err != nil {
			e.logger.Warn().Err(err).
				Str("execution_id", execution.ID).
				Str("step", stepID).
				Msg("llm usage: failed to persist row")
		}
	}
}

// recordStepOutcome persists an outcome row for a completed step.
// Outcome="pending_validation" (the common case) is expected to be
// finalized by the next step's consumer; the finalization paths live
// at the consumer sites (workflow loop, plan_step parse failures,
// terminal sweep). Silent no-op if the repo isn't wired.
func (e *Executor) recordStepOutcome(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	stepID, role, model, outcome, errorClass, errorDetail string,
	attributedToStepID *string,
	duration *int64,
) {
	e.recordStepOutcomeWithSignals(ctx, task, execution, stepID, role, model, outcome, errorClass, errorDetail, attributedToStepID, duration, nil)
}

// agentBudgetStamp carries the three migration-106 columns that are
// populated only for agent-type steps. Non-agent callers leave it
// zero-valued so the fields stay NULL in the database.
type agentBudgetStamp struct {
	ComplexityTier      string // empty → NULL
	EffectiveToolBudget *int   // nil → NULL
	ToolCallsUsed       *int   // nil → NULL
	AgentImageID        string // empty → NULL; immutable runtime-observed ID
	// ContainerExitCode is the container's exit status (migration 169).
	// nil → NULL, meaning no container ran — which is NOT the same as a
	// container that exited 0, so this is never defaulted.
	ContainerExitCode *int
	// ContainerMemoryPeakBytes is the cgroup memory high-water mark the
	// container reported in result.json (migration 204; agent container
	// memory limits design §2.3a). nil → NULL: not reported. Stamped on the
	// ephemeral path only, by stampMemoryPeak.
	ContainerMemoryPeakBytes *int64
	// StepPrompt is the step's first model input as the container wrote it
	// (step_prompt.json), or nil when no file was written — an image predating
	// the contract, or a step that died before its first request. Persisted
	// content-addressed by recordStepOutcomeWithSignalsAndBudget, whose
	// hashes then ride the outcome row (migration 175).
	StepPrompt *stepPromptFile
	// StepIO is the pair of boundary files — task.json as written, result.json
	// as read back after the result_json secrets checkpoint — persisted the
	// same way, whose hashes ride the row's input_hash/result_hash (migration
	// 178, step-I/O persistence design §3). Nil bytes = no file.
	StepIO stepIOFiles
}

// taintStamp carries the three migration-137 taint-lineage columns, populated
// only for agent-type steps (where a toolAudit list exists). Non-agent callers
// (system/plan/gate steps) leave it zero-valued so the columns stay
// false/NULL — correct, since a system step consumes prior steps' taint via
// the lineage rollup rather than calling untrusted tools itself (§5.1). Mirrors
// agentBudgetStamp; the executor marshals StepTaint.Sources at the boundary so
// internal/persistence stays classifier-free (D2).
type taintStamp struct {
	UntrustedContentUsed bool
	UntrustedSources     []byte // JSONB blob (marshalled taintlineage.Source slice); nil → NULL
	RequiresReview       bool
}

// taintStampFromStep marshals a classifier StepTaint into the persistence
// stamp. A step that touched no untrusted content (SeverityNone) yields a zero
// stamp (all columns false/NULL). Best-effort marshal: a failure logs and
// leaves the blob nil but still records the bools, so the audit signal is never
// lost to a serialization hiccup.
func (e *Executor) taintStampFromStep(st taintlineage.StepTaint) taintStamp {
	stamp := taintStamp{
		UntrustedContentUsed: st.Used,
		RequiresReview:       st.RequiresReview,
	}
	// Persist the M1 SourcesBlob (capped display list + full_hash over the
	// step's FULL pre-cap set + drop count), so the lineage latch key reflects
	// per-step overflow. Only for a tainted step (Used); an untainted step
	// leaves the column NULL.
	if st.Used {
		if blob, err := json.Marshal(taintlineage.NewSourcesBlob(st)); err == nil {
			stamp.UntrustedSources = blob
		} else if e != nil {
			e.logger.Warn().Err(err).Msg("taint: failed to marshal untrusted sources blob")
		}
	}
	if st.DroppedSources > 0 && e != nil {
		e.logger.Debug().Int("dropped", st.DroppedSources).
			Msg("taint: capped untrusted source list at MaxSources")
	}
	return stamp
}

// recordStepOutcomeWithSignals is the wider form that also persists
// hallucination-detector findings on the row. Existing call sites
// continue to use recordStepOutcome (which forwards a nil signals
// slice); the agent-step path uses this directly so the JSONB
// column carries the Phase-1 detector output. Kept as a separate
// method so the dozen existing call sites don't churn — they
// don't have signals to persist anyway.
func (e *Executor) recordStepOutcomeWithSignals(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	stepID, role, model, outcome, errorClass, errorDetail string,
	attributedToStepID *string,
	duration *int64,
	signals []byte,
) {
	e.recordStepOutcomeWithSignalsAndBudget(ctx, task, execution, stepID, role, model, outcome, errorClass, errorDetail, attributedToStepID, duration, signals, agentBudgetStamp{}, taintStamp{})
}

// recordStepOutcomeWithSignalsAndBudget is the full form used by the
// agent-step path: in addition to hallucination signals it stamps the
// migration-106 budget triple (complexity tier, effective budget,
// tool calls used). Non-agent callers use recordStepOutcomeWithSignals
// which forwards a zero agentBudgetStamp so the three columns stay NULL.
func (e *Executor) recordStepOutcomeWithSignalsAndBudget(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	stepID, role, model, outcome, errorClass, errorDetail string,
	attributedToStepID *string,
	duration *int64,
	signals []byte,
	budget agentBudgetStamp,
	taint taintStamp,
) {
	if e.outcomeRepo == nil {
		return
	}
	// The recorder is called from the step's own defer, on the step's
	// context. When that context is what ended the step (a max_wall_clock cap,
	// an operator cancel) the row was refused by the database and lost, so the
	// timeout and cancelled classes went uncounted for exactly the steps they
	// exist for (outcome-quality LLD §S2 C1, GitHub #142). Safe because
	// nothing below derives a value from ctx's deadline: the budget columns
	// come from the agentBudgetStamp argument.
	ctx, cancelRecord := finaliserContext(ctx)
	defer cancelRecord()
	entry := &persistence.ExecutionStepOutcome{
		ID:                 persistence.GenerateID("outc"),
		ProjectID:          task.ProjectID,
		TaskID:             task.ID,
		ExecutionID:        execution.ID,
		StepID:             stepID,
		Role:               role,
		Model:              model,
		Outcome:            outcome,
		AttributedToStepID: attributedToStepID,
		ErrorClass:         errorClass,
		// Both ends: the daemon's message is at the head, the container log's
		// decisive last lines at the tail. See truncateDetailPreservingEnds.
		ErrorDetail:          truncateDetailPreservingEnds(errorDetail, 2000),
		DurationMS:           duration,
		RecordedAt:           time.Now().UTC(),
		HallucinationSignals: signals,
		// Migration-106 budget stamp — populated for agent steps only;
		// non-agent steps leave these NULL via a zero agentBudgetStamp.
		ComplexityTier:      budget.ComplexityTier,
		EffectiveToolBudget: budget.EffectiveToolBudget,
		ToolCallsUsed:       budget.ToolCallsUsed,
		AgentImageID:        budget.AgentImageID,
		// Migration-169: the container's exit status, so the residual
		// failure bucket can be grouped by exit code instead of regexed
		// out of error_detail.
		ContainerExitCode: budget.ContainerExitCode,
		// Migration-204: the container's own memory peak; nil stays NULL.
		ContainerMemoryPeakBytes: budget.ContainerMemoryPeakBytes,
		// Migration-137 taint-lineage stamp — populated for agent steps only;
		// non-agent steps leave these false/NULL via a zero taintStamp (§5.1).
		UntrustedContentUsed: taint.UntrustedContentUsed,
		UntrustedSources:     taint.UntrustedSources,
		RequiresReview:       taint.RequiresReview,
	}
	// Stamp the canonical-context source captured at workspace
	// prep so operator queries against context_source surface
	// every step the convention applied to. Empty when the
	// project doesn't use the convention or workspace prep
	// hasn't run for this execution yet (rare; recovery paths).
	if v, ok := e.contextSourceByExecution.Load(execution.ID); ok {
		if s, ok := v.(string); ok {
			entry.ContextSource = s
		}
	}
	if outcome != string(stepoutcome.PendingValidation) && outcome != "" {
		now := time.Now().UTC()
		entry.FinalizedAt = &now
	}
	// Model-visible means persisted: the three parts are stored (redacted at
	// the seam, hash taken after) BEFORE the outcome row, so a row that carries
	// a hash always has its body; a body without a row is what retention
	// removes.
	entry.PromptHashes = e.persistStepPrompt(ctx, execution.ID, stepID, budget.StepPrompt)
	io := e.persistStepIO(ctx, execution.ID, stepID, budget.StepIO)
	entry.PromptHashes.Input, entry.PromptHashes.Result = io.Input, io.Result
	if err := e.outcomeRepo.Record(ctx, entry); err != nil {
		e.logger.Warn().Err(err).
			Str("execution_id", execution.ID).
			Str("step", stepID).
			Str("outcome", outcome).
			Msg("step outcome: failed to persist row")
	} else {
		// Broadcast so the live view can flip this attempt's card to its
		// real state. Retry attempts (stepID_infra_retryN / _shape_retry /
		// _model_fallback) get NO step_completed event — this is the only
		// signal that marks them failed/ok instead of "running" forever
		// (2026-07-12 deep-research live-view report).
		e.emitLive(ctx, execution.ID, livepubsub.KindOutcomeRecorded,
			livepubsub.OutcomeRecordedPayload{StepID: stepID, Class: outcome, Notes: errorClass})
	}
	// Direct-terminal writes (container failure, timeout, cancelled,
	// degenerate_loop) bypass finalizePendingOutcome — emit the
	// Prometheus event here so the quality gauges see them.
	if e.metrics != nil && outcome != string(stepoutcome.PendingValidation) && outcome != "" {
		e.metrics.RecordFinalOutcome(role, model, outcome, errorClass)
	}
}

// finalizePendingOutcome flips a prior step's pending_validation row to
// the given outcome and emits the Prometheus event for the quality
// gauges. ErrNotFound is swallowed — an execution that began before
// this feature landed won't have a pending row, and we don't want the
// finalization to fail loudly just because its write never happened.
func (e *Executor) finalizePendingOutcome(
	ctx context.Context,
	executionID, stepID, outcome, errorClass, errorDetail string,
	attributedToStepID *string,
) {
	if e.outcomeRepo == nil {
		return
	}
	role, model, err := e.outcomeRepo.FinalizePending(
		ctx, executionID, stepID, outcome, errorClass,
		truncateDetailPreservingEnds(errorDetail, 2000), attributedToStepID,
	)
	if err == persistence.ErrNotFound {
		return
	}
	if err != nil {
		e.logger.Warn().Err(err).
			Str("execution_id", executionID).
			Str("step", stepID).
			Str("outcome", outcome).
			Msg("step outcome: failed to finalize pending row")
		return
	}
	if e.metrics != nil {
		e.metrics.RecordFinalOutcome(role, model, outcome, errorClass)
	}
	// Mirror the record-time broadcast: the live view seeded/rendered this
	// step's card off the pending_validation row and needs the flip.
	e.emitLive(ctx, executionID, livepubsub.KindOutcomeRecorded,
		livepubsub.OutcomeRecordedPayload{StepID: stepID, Class: outcome, Notes: errorClass})
}

// sweepPendingOutcomes finalizes any lingering pending_validation rows
// for an execution and emits a Prometheus event per swept row. The
// per-row emission is deliberate: without it, the last step of every
// execution (which by definition has no consumer to finalize it
// explicitly) would never land in the quality gauges. Non-fatal.
func (e *Executor) sweepPendingOutcomes(ctx context.Context, executionID, fallbackOutcome string) {
	if e.outcomeRepo == nil {
		return
	}
	swept, err := e.outcomeRepo.SweepPending(ctx, executionID, fallbackOutcome)
	if err != nil {
		e.logger.Warn().Err(err).
			Str("execution_id", executionID).
			Str("fallback", fallbackOutcome).
			Msg("step outcome: sweep failed")
		return
	}
	if len(swept) > 0 {
		e.logger.Debug().
			Str("execution_id", executionID).
			Str("fallback", fallbackOutcome).
			Int("count", len(swept)).
			Msg("step outcome: swept pending rows")
		if e.metrics != nil {
			for _, r := range swept {
				// A swept row carries no error class, so it never counts as
				// a failure not charged to the model.
				e.metrics.RecordFinalOutcome(r.Role, r.Model, fallbackOutcome, "")
			}
		}
	}
}

// auditEntryForDetection is the minimal shape the degenerate-loop
// detector needs from a tool audit entry. Kept small and copyable so
// tests can build inputs inline.
type auditEntryForDetection struct {
	Tool  string
	Input string
}

// degenerateLoopThreshold is the minimum run length of identical
// (tool, input) calls to classify as a degenerate loop. Three matches
// an agent that's stuck re-reading the same file, re-running the same
// search, or re-trying the same command — clear runaway behavior, not
// a coincidence.
const degenerateLoopThreshold = 3

// nearDegenerateLoopThreshold is the run length required when the calls match
// only after digits are normalised away. Higher than the exact threshold
// because a repeating SHAPE can be legitimate — paging through a large file is
// the honest version of the same pattern — while byte-identical repeats never
// are. Mirrors NEAR_MAX_REPEATS in images/vornik-agent/entrypoint.sh so the
// label this classifier writes agrees with the decision the container made.
const nearDegenerateLoopThreshold = 12

// normaliseLoopInput collapses every run of digits to "#" so calls that differ
// only in a number compare equal.
//
// Measured 2026-08-18: a coder walked a file with `sed -n '175,270p'`, then
// '176,280p', then '177,290p', one line further per iteration. The exact
// comparison never matched, so the step exhausted its 125-iteration budget and
// was labelled iteration_exhausted — hiding a loop as a budget problem, and
// with it the read-only-git root cause underneath.
func normaliseLoopInput(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	inDigits := false
	for _, r := range in {
		if r >= '0' && r <= '9' {
			if !inDigits {
				b.WriteByte('#')
				inDigits = true
			}
			continue
		}
		inDigits = false
		b.WriteRune(r)
	}
	return b.String()
}

// detectDegenerateLoop scans audit entries for a run of N consecutive
// identical (tool, input) calls. Returns a descriptive detail string
// when a loop is found, else "". Non-consecutive repeats are not a
// loop — a healthy agent often revisits the same tool over the course
// of a step (e.g. read → edit → read), just not with identical inputs
// back-to-back.
func detectDegenerateLoop(entries []auditEntryForDetection) string {
	if len(entries) < degenerateLoopThreshold {
		return ""
	}
	runLen := 1
	runTool := entries[0].Tool
	runInput := entries[0].Input
	nearLen := 1
	nearTool := entries[0].Tool
	nearInput := normaliseLoopInput(entries[0].Input)
	for i := 1; i < len(entries); i++ {
		e := entries[i]
		if e.Tool == runTool && e.Input == runInput {
			runLen++
			if runLen >= degenerateLoopThreshold {
				preview := runInput
				if len(preview) > 200 {
					preview = preview[:200] + "..."
				}
				return fmt.Sprintf("repeated %s call %d times with identical input: %s",
					runTool, runLen, preview)
			}
		} else {
			runLen = 1
			runTool = e.Tool
			runInput = e.Input
		}
		// Near-run tracked independently: a sliding window never repeats
		// exactly, so the exact counter above resets on every call while this
		// one climbs.
		if norm := normaliseLoopInput(e.Input); e.Tool == nearTool && norm == nearInput {
			nearLen++
			if nearLen >= nearDegenerateLoopThreshold {
				preview := e.Input
				if len(preview) > 200 {
					preview = preview[:200] + "..."
				}
				return fmt.Sprintf("repeated %s call %d times with near-identical input (differing only in numbers): %s",
					nearTool, nearLen, preview)
			}
		} else {
			nearLen = 1
			nearTool = e.Tool
			nearInput = normaliseLoopInput(e.Input)
		}
	}
	return ""
}

// clampToolAuditDurationMs is the executor-side wrapper around the
// shared persistence.ClampToolAuditDurationMs. Three writers feed the
// audit log (api/tool_audit_handlers.go, this file, and dispatcher/
// tools.go) and they share the bounds check via persistence so a
// future tweak doesn't drift across packages — observed 2026-05-08:
// only the batch path clamped, so -1.6e12 ms still landed via the
// realtime stream. The wrapper exists to log the original value
// before clamping so the underlying agent bug doesn't go silent.
func clampToolAuditDurationMs(e *Executor, tool, executionID string, ms int64) int64 {
	clamped := persistence.ClampToolAuditDurationMs(ms)
	if clamped != ms && e != nil {
		e.logger.Warn().
			Str("execution_id", executionID).
			Str("tool", tool).
			Int64("reported_ms", ms).
			Int64("clamped_to", clamped).
			Msg("audit: tool duration_ms outside sane range — clamping (likely agent ms_now() drift)")
	}
	return clamped
}

// recordToolCallCounts emits the per-tool Prometheus counters for a step's
// audited calls. No-op when metrics are unwired.
func (e *Executor) recordToolCallCounts(projectID string, entries []auditEntryForDetection) {
	if e.metrics == nil {
		return
	}
	toolCounts := make(map[string]int, len(entries))
	for _, entry := range entries {
		toolCounts[entry.Tool]++
	}
	e.metrics.RecordToolCalls(projectID, toolCounts)
}

// recordStepTaintMetric increments the per-severity step-taint counter (§8).
// No-op when metrics are unwired.
func (e *Executor) recordStepTaintMetric(st taintlineage.StepTaint) {
	if e.metrics == nil || e.metrics.StepTaintTotal == nil {
		return
	}
	e.metrics.StepTaintTotal.WithLabelValues(st.MaxSeverity.String()).Inc()
}

// persistToolAuditFromResult parses toolAudit from raw result.json bytes,
// writes entries to the audit log, runs the degenerate-loop detector, and
// classifies the step's untrusted-content taint from the same tool
// names/inputs (taint-lineage-tracking §4.2). Returns the tool-call count (for
// the migration-106 budget stamp), a non-empty loop-detail string when a
// degenerate loop is detected (the caller uses it to override the default
// pending_validation outcome in executeAgentStep's defer), and the classified
// StepTaint (migration-137 stamp). A parse failure / empty audit returns a zero
// StepTaint (untainted) — correct, a step that ran no tools consumed no
// untrusted content.
func (e *Executor) persistToolAuditFromResult(ctx context.Context, task *persistence.Task, execution *persistence.Execution, stepID string, resultBytes []byte) (toolCallCount int, loopDetail string, taint taintlineage.StepTaint) {
	var parsed struct {
		ToolAudit []struct {
			AuditID    string `json:"audit_id"`
			Tool       string `json:"tool"`
			Input      string `json:"input"`
			Output     string `json:"output"`
			DurationMs int64  `json:"duration_ms"`
		} `json:"toolAudit"`
	}
	if err := json.Unmarshal(resultBytes, &parsed); err != nil {
		e.logger.Warn().Err(err).Str("execution_id", execution.ID).
			Int("result_len", len(resultBytes)).
			Msg("audit: failed to parse result.json")
		return 0, "", taintlineage.StepTaint{}
	}
	if len(parsed.ToolAudit) == 0 {
		e.logger.Debug().Str("execution_id", execution.ID).Str("step", stepID).
			Msg("audit: no toolAudit entries in result.json")
		return 0, "", taintlineage.StepTaint{}
	}

	toolCallCount = len(parsed.ToolAudit)

	// Degenerate-loop detection + taint classification both run regardless of
	// whether the audit repo is wired, because both drive outcome
	// classification (degenerate_loop outcome / taint columns), not just DB
	// logging. Build both input slices in the one loop over the audit entries.
	detected := make([]auditEntryForDetection, 0, len(parsed.ToolAudit))
	toolCalls := make([]taintlineage.ToolCall, 0, len(parsed.ToolAudit))
	for _, entry := range parsed.ToolAudit {
		detected = append(detected, auditEntryForDetection{Tool: entry.Tool, Input: entry.Input})
		toolCalls = append(toolCalls, taintlineage.ToolCall{Tool: entry.Tool, Input: entry.Input})
	}
	taint = taintlineage.Classify(toolCalls)
	if e.metrics != nil {
		e.recordStepTaintMetric(taint)
	}
	loopDetail = detectDegenerateLoop(detected)
	if loopDetail != "" {
		e.logger.Warn().
			Str("execution_id", execution.ID).
			Str("step", stepID).
			Str("detail", loopDetail).
			Msg("audit: degenerate tool loop detected")
	}

	if e.auditRepo == nil {
		e.logger.Debug().Str("execution_id", execution.ID).Msg("audit: repo is nil, skipping persist")
		return toolCallCount, loopDetail, taint
	}

	e.logger.Info().Str("execution_id", execution.ID).Str("step", stepID).
		Int("entries", len(parsed.ToolAudit)).
		Msg("audit: persisting tool audit entries")

	// Record tool call metrics for Prometheus (per-tool counts from the
	// already-built detection slice, avoiding a second pass over parsed).
	e.recordToolCallCounts(task.ProjectID, detected)

	for _, entry := range parsed.ToolAudit {
		// Secret redaction for this sink moved to the repository seam
		// (internal/auditredact) on 2026-08-20. Scanning here cleaned only
		// THIS writer's row, and because the INSERT is ON CONFLICT (id) DO
		// NOTHING and the agent's realtime stream shares the same audit_id,
		// the unredacted realtime row usually won the race and this one was
		// discarded. The decorator covers every writer, so scanning again here
		// would be a second copy of the policy with no effect.
		input, output := entry.Input, entry.Output
		// Reuse the agent-supplied audit_id when present so the
		// realtime stream (POST /api/v1/internal/tool-audit) and
		// this post-step batch dedup cleanly via ON CONFLICT DO
		// NOTHING on the (id) PK. Pre-2026.5 agents that don't
		// emit audit_id fall through to a fresh ID — duplicates
		// only become possible if such an agent ALSO learns to
		// stream, which it can't without the entrypoint update.
		id := entry.AuditID
		if id == "" {
			id = persistence.GenerateID("ta")
		}
		auditEntry := &persistence.ToolAuditEntry{
			ID:          id,
			ProjectID:   task.ProjectID,
			TaskID:      task.ID,
			ExecutionID: execution.ID,
			StepID:      stepID,
			ToolName:    entry.Tool,
			ToolInput:   input,
			ToolOutput:  output,
			DurationMs:  clampToolAuditDurationMs(e, entry.Tool, execution.ID, entry.DurationMs),
			CreatedAt:   time.Now(),
		}
		if err := e.auditRepo.Log(ctx, auditEntry); err != nil {
			e.logger.Error().Err(err).Str("tool", entry.Tool).Str("execution_id", execution.ID).
				Int64("duration_ms", auditEntry.DurationMs).
				Msg("audit: failed to write audit entry to DB")
		}
		// Credential carryover: capture an operator-facing access credential
		// from this tool's RAW output (not the redacted copy) when the tool
		// matches a configured mapping. Best-effort; never fails the step.
		e.captureToolCredential(ctx, task, execution, entry.Tool, entry.Output)
	}
	return toolCallCount, loopDetail, taint
}
