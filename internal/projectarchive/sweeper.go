// Package projectarchive runs the archived-project deletion
// sweeper. When an operator archives a project the UI writes a
// `lifecycle:` block into the project YAML with a
// scheduledDeleteAt timestamp; this package's Sweeper scans the
// registry on a cadence and, for each project whose scheduled
// time has elapsed, hard-deletes the YAML files, every project-
// scoped DB row, and every artifact blob on disk.
//
// Idempotent: re-running over a partially-deleted project does
// not error. The sweeper is the SINGLE deletion point; the UI's
// "Delete now" button kicks SweepNow rather than running the
// deletion path inline, so audit / reload / error handling share
// one code path.
package projectarchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/safepath"
)

// DefaultTickInterval is the cadence between automatic sweeps.
// Hourly hits the right balance between operator UX (a 7-day
// grace doesn't need second-precision) and pressure on the DB
// (a full table scan over the project_data tables every minute
// would be noisy). The interval can be overridden via Config.
const DefaultTickInterval = 1 * time.Hour

// ReloadFunc reloads the registry from disk so the in-memory
// map drops the deleted project. Nil-safe in NewSweeper.
type ReloadFunc func(ctx context.Context) error

// ArtifactWiper is the narrow contract the sweeper uses to delete
// a project's blob storage. Satisfied by *artifacts.Store via its
// DeleteProjectArtifacts method. Optional — when nil the sweeper
// falls back to ArtifactBasePath filesystem removal (legacy
// direct-disk deployments without a backend-aware store).
type ArtifactWiper interface {
	DeleteProjectArtifacts(ctx context.Context, projectID string) (int, error)
}

// Config wires the sweeper. Registry + DataDeleter are required;
// everything else is optional and degrades to "skip that step"
// when nil.
type Config struct {
	Registry         *registry.Registry
	DataDeleter      persistence.ProjectDataDeleter
	ConfigDir        string        // root of configs/ — projects YAML lives under <ConfigDir>/projects/
	ArtifactBasePath string        // legacy direct-disk fallback when ArtifactWiper is nil
	ArtifactWiper    ArtifactWiper // backend-aware blob deletion (preferred; works against both filesystem AND S3)
	AuditRepo        persistence.AdminAuditRepository
	Reload           ReloadFunc
	TickInterval     time.Duration
	Logger           zerolog.Logger
	Clock            func() time.Time // injectable for tests; nil → time.Now
	// LeaderGate, when non-nil, is consulted before each tick.
	// IsLeader()=false → skip this tick + log at debug level.
	// Used by multi-replica deployments to ensure exactly one
	// daemon is wiping projects at a time (2026.8.0
	// horizontal-scaling prep). Nil leaves the gate open —
	// single-process deployments don't need election.
	LeaderGate LeaderGate
}

// LeaderGate is the narrow contract the sweeper consults to
// decide whether THIS daemon should be running the worker.
// Satisfied by *leaderelection.Elector; defined as an
// interface here to avoid pulling the leaderelection package
// into projectarchive's dependency set.
type LeaderGate interface {
	IsLeader() bool
}

// Sweeper is the deletion runner. Construct via NewSweeper, drive
// via Run (long-lived goroutine) or SweepNow (one-shot kick).
type Sweeper struct {
	cfg Config

	// kickCh forces a sweep mid-tick. SweepNow drops a token here;
	// a buffered channel size of 1 collapses overlapping kicks
	// into one upcoming sweep.
	kickCh chan struct{}

	// mu guards inflight to enforce serialised sweeps — overlapping
	// runs against the same project would race on the YAML and DB.
	mu       sync.Mutex
	inflight bool

	// Per-project deletion outcomes (GitHub #365): what the caller of
	// delete-now, and the project banner, are told. outMu guards all four;
	// outCh is closed and replaced on every record, waking WaitOutcome.
	outMu    sync.Mutex
	outSeq   uint64
	outcomes map[string]DeleteOutcome
	outCh    chan struct{}
}

// DeleteOutcome is the result of one attempt to delete a project: Err is nil
// when every step of deleteProject succeeded. Seq orders outcomes across all
// projects (a counter, not a clock), so a caller can wait for one recorded
// after a point it captured.
type DeleteOutcome struct {
	Seq uint64
	At  time.Time
	Err error
	// Residue is set, with Err nil, when the project itself was removed (its
	// DB rows and YAML are gone) but a later step was not done: some artifact
	// blobs could not be deleted, or the registry reload failed (archival
	// design section H, 2026-10-14). The project no longer exists for the
	// operator; nothing retries the blobs.
	Residue error
}

// recordOutcome stores projectID's outcome and wakes every waiter. It is
// called once per attempted project, after deleteProject returns — so after
// the DB wipe, the artifact and YAML removal, the reload and the audit row:
// "deleted" is never reported for a partly-done sequence.
func (s *Sweeper) recordOutcome(projectID string, err, residue error) {
	clock := s.cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.initOutcomesLocked()
	s.outSeq++
	o := DeleteOutcome{Seq: s.outSeq, At: clock(), Err: err}
	if err == nil {
		o.Residue = residue
	}
	s.outcomes[projectID] = o
	close(s.outCh)
	s.outCh = make(chan struct{})
}

func (s *Sweeper) initOutcomesLocked() {
	if s.outcomes == nil {
		s.outcomes = map[string]DeleteOutcome{}
	}
	if s.outCh == nil {
		s.outCh = make(chan struct{})
	}
}

// OutcomeSeq returns the sequence number of the latest recorded outcome. A
// caller that wants the result of the deletion it is about to trigger reads it
// BEFORE triggering, then passes it to WaitOutcome.
func (s *Sweeper) OutcomeSeq() uint64 {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.outSeq
}

// LastOutcome returns the most recent outcome recorded for projectID. Held in
// memory only; the durable record is the project.deleted /
// project.delete.failed audit row.
func (s *Sweeper) LastOutcome(projectID string) (DeleteOutcome, bool) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	o, ok := s.outcomes[projectID]
	return o, ok
}

// WaitOutcome blocks until an outcome for projectID with Seq > afterSeq is
// recorded, or ctx ends (false). Any attempt on the project after afterSeq
// answers — the kicked sweep, or an hourly tick that reached it first: both
// are attempts to delete that project, and the first to finish is its state.
func (s *Sweeper) WaitOutcome(ctx context.Context, projectID string, afterSeq uint64) (DeleteOutcome, bool) {
	for {
		s.outMu.Lock()
		s.initOutcomesLocked()
		if o, ok := s.outcomes[projectID]; ok && o.Seq > afterSeq {
			s.outMu.Unlock()
			return o, true
		}
		ch := s.outCh
		s.outMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return DeleteOutcome{}, false
		}
	}
}

// NewSweeper builds a Sweeper with defaults applied. Required
// fields (Registry, DataDeleter) get nil-checked at the use site
// in sweepOnce; a nil-fielded Sweeper is still safe to Run (it
// just logs and skips).
func NewSweeper(cfg Config) *Sweeper {
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = DefaultTickInterval
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Sweeper{
		cfg:    cfg,
		kickCh: make(chan struct{}, 1),
	}
}

// Run blocks on a ticker until ctx is cancelled. Each tick (and
// each SweepNow kick) calls sweepOnce.
func (s *Sweeper) Run(ctx context.Context) {
	s.cfg.Logger.Info().Dur("interval", s.cfg.TickInterval).Msg("archive-sweeper: started")
	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()
	// Run once at startup so an archived project past its grace
	// gets deleted promptly after a daemon restart.
	s.sweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			s.cfg.Logger.Info().Msg("archive-sweeper: stopped")
			return
		case <-ticker.C:
			s.sweepOnce(ctx)
		case <-s.kickCh:
			s.sweepOnce(ctx)
		}
	}
}

// SweepNow kicks an out-of-band sweep. Idempotent — overlapping
// calls collapse to one upcoming sweep. Safe to call from any
// goroutine.
func (s *Sweeper) SweepNow(ctx context.Context) {
	select {
	case s.kickCh <- struct{}{}:
	default:
		// Already pending — collapse.
	}
}

// sweepOnce runs one pass over the registry's archived
// projects. Each due project is deleted; non-due ones are left
// alone. Errors are logged + audited but don't stop the pass
// (one stuck project shouldn't block the rest).
func (s *Sweeper) sweepOnce(ctx context.Context) {
	if s.cfg.Registry == nil {
		s.cfg.Logger.Warn().Msg("archive-sweeper: registry not wired; skipping tick")
		return
	}
	// Leader gate: in a multi-replica deployment only the
	// elected leader runs the wipe. Other replicas skip the
	// tick entirely — no DB query, no audit row, no log noise.
	// Nil gate means single-process deployment; tick runs as
	// usual.
	if s.cfg.LeaderGate != nil && !s.cfg.LeaderGate.IsLeader() {
		s.cfg.Logger.Debug().Msg("archive-sweeper: not the leader; skipping tick")
		return
	}
	s.mu.Lock()
	if s.inflight {
		s.mu.Unlock()
		return
	}
	s.inflight = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight = false
		s.mu.Unlock()
	}()

	now := s.cfg.Clock()
	archived := s.cfg.Registry.ListArchivedProjects()
	if len(archived) == 0 {
		return
	}
	due := 0
	for _, p := range archived {
		if p == nil {
			continue
		}
		if !p.DeletionDue(now) {
			continue
		}
		due++
		residue, err := s.deleteProject(ctx, p)
		if err != nil {
			s.cfg.Logger.Error().Err(err).Str("project_id", p.ID).Msg("archive-sweeper: delete failed")
			s.audit(ctx, "project.delete.failed", p.ID, map[string]any{
				"error": err.Error(),
			})
		}
		s.recordOutcome(p.ID, err, residue)
	}
	if due > 0 {
		s.cfg.Logger.Info().Int("due", due).Msg("archive-sweeper: pass complete")
	}
}

// deleteProject executes the full wipe for one project:
//
//  1. DB rows via the ProjectDataDeleter.
//  2. Artifact blobs under <ArtifactBasePath>/<projectID>/.
//  3. Project YAML + companion PROJECT.md under
//     <ConfigDir>/projects/.
//  4. Registry reload so the deleted project disappears from
//     in-memory state.
//  5. Audit row recording the scope of the wipe.
//
// Step 1 first so a half-wiped project keeps a YAML row that the
// next sweeper tick can re-attempt against. Step 4 last so the
// audit row writes against the project ID that just disappeared.
//
// err is a failure that leaves the project in place (DB wipe, YAML removal):
// it stays archived and the next tick retries. residue is what was not done
// once the project itself was removed — artifact blobs left behind, a failed
// reload — answered as deleted_with_errors and audited as
// project.delete.partial, never as project.delete.failed (archival design
// section H, 2026-10-14).
func (s *Sweeper) deleteProject(ctx context.Context, p *registry.Project) (residue, err error) {
	stats := persistence.ProjectDataStats{}
	var artifactErr, reloadErr error
	artifactBase := ""

	if s.cfg.DataDeleter != nil {
		st, err := s.cfg.DataDeleter.DeleteProjectData(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("db wipe: %w", err)
		}
		stats = st
	}

	artifactsRemoved := false
	artifactsCount := 0
	switch {
	case s.cfg.ArtifactWiper != nil:
		// Preferred path: works against both filesystem and S3
		// backends. Walks the backend's project-key prefix and
		// deletes each blob.
		n, err := s.cfg.ArtifactWiper.DeleteProjectArtifacts(ctx, p.ID)
		if err != nil {
			artifactErr = &ResidueError{Step: ResidueArtifacts, Err: fmt.Errorf("artifact wipe: %w", err)}
		}
		artifactsCount = n
		artifactsRemoved = n > 0
	case s.cfg.ArtifactBasePath != "":
		// Legacy direct-disk fallback. Used by deployments that
		// wire the sweeper without going through artifacts.Store
		// (mostly test fixtures these days). Local filesystem
		// only; S3 deployments need ArtifactWiper.
		artifactBase = s.cfg.ArtifactBasePath
		artDir := filepath.Join(s.cfg.ArtifactBasePath, p.ID)
		if err := safeRemoveAll(artDir, s.cfg.ArtifactBasePath); err != nil {
			artifactErr = &ResidueError{Step: ResidueArtifacts, Err: fmt.Errorf("artifact wipe: %w", err)}
		} else {
			artifactsRemoved = true
		}
	}

	yamlRemoved, mdRemoved, err := s.removeProjectFiles(p.ID)
	if err != nil {
		if artifactErr != nil {
			return nil, fmt.Errorf("yaml remove: %w (db_rows=%d; also %v)", err, stats.RowsDeleted, artifactErr)
		}
		return nil, fmt.Errorf("yaml remove: %w (db_rows=%d artifact_dir_removed=%v)", err, stats.RowsDeleted, artifactsRemoved)
	}

	if s.cfg.Reload != nil {
		if err := s.cfg.Reload(ctx); err != nil {
			reloadErr = &ResidueError{Step: ResidueReload, Err: fmt.Errorf("reload: %w", err)}
		}
	}

	// A wipe that left the derived vectors behind is not the deletion the row
	// claims, so the row says what happened to them. Both fields, always: a bare
	// zero cannot distinguish "nothing was derived from this project" from "no
	// evictor was wired and nobody looked".
	// On SQLite there is no embedding_cache, so nothing derived was stored
	// and the deleter says so (CacheEvictionNotApplicable, GitHub #365).
	if !stats.CacheEvictionRan && !stats.CacheEvictionNotApplicable && s.cfg.DataDeleter != nil {
		s.cfg.Logger.Warn().Str("project_id", p.ID).
			Msg("archive-sweeper: project wiped with NO embedding-cache eviction; " +
				"vectors derived from the deleted content remain")
	}

	s.audit(ctx, "project.deleted", p.ID, map[string]any{
		"tables_cleared":                stats.TablesCleared,
		"rows_deleted":                  stats.RowsDeleted,
		"cache_eviction_ran":            stats.CacheEvictionRan,
		"cache_eviction_not_applicable": stats.CacheEvictionNotApplicable,
		"cached_embeddings_evicted":     stats.CachedEmbeddingsEvicted,
		"artifacts_removed":             artifactsRemoved,
		"artifacts_count":               artifactsCount,
		"yaml_removed":                  yamlRemoved,
		"projectmd_removed":             mdRemoved,
		"scheduled_delete_at":           yamlScheduledDeleteAtOf(p),
	})

	residue = errors.Join(artifactErr, reloadErr)
	if residue != nil {
		s.recordResidue(ctx, p.ID, artifactBase, artifactsCount, artifactErr, reloadErr)
	}
	return residue, nil
}

// ResidueStep names a deletion step left undone after the project was
// removed (archival design section H.5).
type ResidueStep string

const (
	// ResidueArtifacts means artifact blobs may remain under the project's key
	// prefix. Nothing retries them once the project is gone.
	ResidueArtifacts ResidueStep = "artifacts"
	// ResidueReload means the registry reload after the YAML removal failed. The
	// archived project stays in the in-memory registry, still due, so the
	// next tick re-runs the (idempotent) deletion, reload included.
	ResidueReload ResidueStep = "reload"
)

// ResidueError is one residue step's error. Error() is the wrapped error's
// text unchanged ("artifact wipe: ..." / "reload: ..."), so logs and audit
// rows read as before; the step is for the surfaces that word it.
type ResidueError struct {
	Step ResidueStep
	Err  error
}

func (e *ResidueError) Error() string { return e.Err.Error() }
func (e *ResidueError) Unwrap() error { return e.Err }

// ErrResidueArtifacts and ErrResidueReload match, through errors.Is, a
// residue that names that step, wherever it sits in a joined error.
var (
	ErrResidueArtifacts = errors.New("residue: artifacts")
	ErrResidueReload    = errors.New("residue: reload")
)

// Is lets errors.Is find the step's sentinel.
func (e *ResidueError) Is(target error) bool {
	switch e.Step {
	case ResidueArtifacts:
		return target == ErrResidueArtifacts
	case ResidueReload:
		return target == ErrResidueReload
	}
	return false
}

// ResidueSteps lists the steps a residue error names, artifacts before
// reload: a fixed order (by sentinel) that matches deleteProject's today. A
// residue that carries no step (an older or foreign error) yields none.
func ResidueSteps(err error) []ResidueStep {
	var out []ResidueStep
	if errors.Is(err, ErrResidueArtifacts) {
		out = append(out, ResidueArtifacts)
	}
	if errors.Is(err, ErrResidueReload) {
		out = append(out, ResidueReload)
	}
	return out
}

// ResidueWarning words a deleted_with_errors residue for the operator, per
// step (archival design section H.5, 2026-10-14). The artifact wording is
// the one section H shipped, kept byte-identical so an older vornikctl that
// strips its prefix still prints it.
func ResidueWarning(err error) string {
	detail := ""
	if err != nil {
		detail = ": " + err.Error()
	}
	steps := ResidueSteps(err)
	switch {
	case len(steps) == 2:
		return "project removed; artifact files may remain and the registry reload failed " +
			"(the archived project stays listed until the sweeper's next tick, which " +
			"completes the deletion and retries the artifact files)" + detail
	case len(steps) == 1 && steps[0] == ResidueReload:
		return "project removed; the registry reload failed, so the archived project stays " +
			"listed until the sweeper's next tick, which completes the deletion" + detail
	case len(steps) == 1 && steps[0] == ResidueArtifacts:
		return "project removed; artifact files may remain" + detail
	default:
		return "project removed; a later deletion step was not done" + detail
	}
}

// residueTotal counts deletions that removed the project but left a step
// undone, by step (archival design section H). Artifacts left behind are
// orphaned: nothing retries them once the project is gone.
var residueTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "vornik", Name: "project_delete_residue_total",
	Help: "Project deletions that removed the project but left a step undone, by step (artifacts: blobs may remain under the project's key prefix; reload: the registry reload failed and the next tick retries it). Each is audited as project.delete.partial.",
}, []string{"step"})

// recordResidue makes a residue findable later: an ERROR line naming the
// prefix and base path, the residue counter, and the project.delete.partial
// audit row — the durable record of which prefix holds orphaned blobs.
func (s *Sweeper) recordResidue(ctx context.Context, projectID, artifactBase string, deletedBeforeErr int, artifactErr, reloadErr error) {
	prefix := projectID + "/"
	extras := map[string]any{
		"error":           errors.Join(artifactErr, reloadErr).Error(),
		"artifact_prefix": prefix,
	}
	if artifactErr != nil {
		residueTotal.WithLabelValues("artifacts").Inc()
		extras["artifacts_deleted_before_error"] = deletedBeforeErr
		if artifactBase != "" {
			extras["artifact_base"] = artifactBase
		}
		s.cfg.Logger.Error().Err(artifactErr).Str("project_id", projectID).
			Str("artifact_prefix", prefix).Str("artifact_base", artifactBase).
			Int("deleted_before_error", deletedBeforeErr).
			Msg("archive-sweeper: project deleted but its artifact blobs were not all removed; they are orphaned under this prefix (audited as project.delete.partial)")
	}
	if reloadErr != nil {
		residueTotal.WithLabelValues("reload").Inc()
		extras["reload_error"] = reloadErr.Error()
		s.cfg.Logger.Error().Err(reloadErr).Str("project_id", projectID).
			Msg("archive-sweeper: project files removed but the registry reload failed; the next tick retries")
	}
	s.audit(ctx, "project.delete.partial", projectID, extras)
}

// removeProjectFiles unlinks the project YAML + companion
// PROJECT.md from the configs directory. Missing files are
// tolerated (idempotent for retries). Returns booleans so the
// audit row distinguishes "didn't exist" from "removed".
func (s *Sweeper) removeProjectFiles(projectID string) (yamlRemoved, mdRemoved bool, err error) {
	if s.cfg.ConfigDir == "" {
		// No configs dir wired — caller is using us purely for
		// DB wipes. Leave the file removal to operator-side
		// tools.
		return false, false, nil
	}
	if strings.ContainsAny(projectID, `/\`) {
		return false, false, fmt.Errorf("refusing to delete project with path-separator in id %q", projectID)
	}
	yamlPath := filepath.Join(s.cfg.ConfigDir, "projects", projectID+".yaml")
	mdPath := filepath.Join(s.cfg.ConfigDir, "projects", projectID+".md")

	if err := safeRemove(yamlPath, s.cfg.ConfigDir); err != nil {
		return false, false, fmt.Errorf("remove %s: %w", yamlPath, err)
	}
	yamlRemoved = true

	if err := safeRemove(mdPath, s.cfg.ConfigDir); err != nil {
		// PROJECT.md is optional — a not-found error is fine.
		if errors.Is(err, os.ErrNotExist) {
			return yamlRemoved, false, nil
		}
		return yamlRemoved, false, fmt.Errorf("remove %s: %w", mdPath, err)
	}
	mdRemoved = true
	return yamlRemoved, mdRemoved, nil
}

// safeRemove unlinks a file after verifying its absolute path
// sits under root. Belt-and-braces against a future bug that
// computes a path traversal — a stray "../" would otherwise let
// the sweeper delete files outside the configs dir.
func safeRemove(path, root string) error {
	if err := assertWithinRoot(path, root); err != nil {
		return err
	}
	err := os.Remove(path)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// safeRemoveAll mirrors safeRemove for directory trees (used by
// the artifact blob wipe).
func safeRemoveAll(path, root string) error {
	if err := assertWithinRoot(path, root); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// assertWithinRoot fails when path doesn't resolve under root.
// Both inputs are Abs'd + cleaned so symlinks / "../" sequences
// can't slip past. Sweep paths are constructed from operator-
// controlled config values — the operator is trusted, but this
// guard catches accidental misconfiguration ("ConfigDir=/" would
// otherwise wipe the entire filesystem).
func assertWithinRoot(path, root string) error {
	// Keep the explicit empty-root guard: safepath.AssertUnder is fail-OPEN on
	// an empty base (returns nil), which would defeat the "ConfigDir=/" misconfig
	// protection this helper exists for.
	if root == "" {
		return fmt.Errorf("empty root")
	}
	// AssertUnder does not Abs its inputs, so pre-absolutize (sweeper roots are
	// absolute in deployment; this keeps the behavior for a relative root/path).
	// AssertUnder then symlink-resolves base + the path's existing prefix — a
	// strict improvement over the prior lexical check for a delete path. Note it
	// also rejects path==root (rel=="."), which the old check allowed; real
	// callers always pass <root>/projects/<id>… sub-paths, never the root itself.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("abs root: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("abs path: %w", err)
	}
	return safepath.AssertUnder(absRoot, absPath)
}

// audit best-effort writes an admin-audit row. Nil-safe.
func (s *Sweeper) audit(ctx context.Context, action, projectID string, extras map[string]any) {
	if s.cfg.AuditRepo == nil {
		return
	}
	after, _ := json.Marshal(extras)
	_ = s.cfg.AuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: "system:archive-sweeper",
		Source:    "sweeper",
		Action:    action,
		Target:    projectID,
		After:     string(after),
	})
}

// yamlScheduledDeleteAtOf reads the project's scheduled deletion
// timestamp for the audit row. Returns empty string when the
// YAML didn't carry one.
func yamlScheduledDeleteAtOf(p *registry.Project) string {
	t, ok := p.ScheduledDeletion()
	if !ok {
		return ""
	}
	return t.Format(time.RFC3339)
}
