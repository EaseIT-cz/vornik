package memory

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// The provenance sweep (design 22, "Amendment 2026-10-21 (#482)", part 3):
// re-runs the rule for every stored provenance record, so a verified chunk
// whose commit left origin/main goes unverified, one whose origin blob
// changed goes stale (and is re-verified on later sweeps), and a pending or
// transiently failed record is verified once the clone holds the merge. It
// needs no artifact bytes: a record holds the stored bytes' blob id.

// ProvenanceSweepActor is the actor of the sweep's audited events.
const ProvenanceSweepActor = "provenance-sweep"

// DefaultProvenanceSweepInterval is the sweep cadence.
const DefaultProvenanceSweepInterval = time.Hour

// ProvenanceSweepStore is what the sweep needs of the verification
// repository.
type ProvenanceSweepStore interface {
	ListProvenanceProjects(ctx context.Context) ([]string, error)
	ListProvenance(ctx context.Context, projectID, afterArtifactID string, limit int) ([]*persistence.ChunkProvenance, error)
	UpsertProvenance(ctx context.Context, p *persistence.ChunkProvenance) error
	ApplyVerificationChange(ctx context.Context, ev *persistence.MemoryVerificationEvent, changes []persistence.ArtifactVerificationChange) error
}

// ProvenanceSweeper runs the sweep.
type ProvenanceSweeper struct {
	Store      ProvenanceSweepStore
	Verifier   *ProvenanceVerifier
	Interval   time.Duration
	Logger     zerolog.Logger
	LeaderGate LeaderGate
	// Clones lists the configured daemon-only clones (memory.provenance_clone,
	// GitHub #499); each is fetched before the pass. Nil = none.
	Clones func() []ProvenanceClone
	// FetchTimeout bounds one clone's fetch (DefaultProvenanceFetchTimeout).
	FetchTimeout time.Duration
	// Metrics carries memory_provenance_fetch_total. Nil = not counted.
	Metrics *Metrics
	// Supersede runs document supersession for (project, scope, path)
	// (#506 C9). The sweep calls it for every provenance-verified record, so a
	// version that verifies after its ingest, a draft pushed later, retires
	// its predecessor; it is idempotent, so a crash between a stamp and its
	// supersession is repaired by the next pass. Nil = not run.
	Supersede func(ctx context.Context, projectID, scope, path string) (int, error)
	// PendingInterval is the cadence of the short pass over drafts awaiting
	// their push (#506 C9.2; DefaultProvenancePendingInterval).
	PendingInterval time.Duration

	fetchState provenanceFetchState
	healMu     sync.Mutex
	healed     map[string]bool
}

// DefaultProvenancePendingInterval is how often the short pass looks for
// pending drafts that are due (#506 C9.2).
const DefaultProvenancePendingInterval = time.Minute

// provenancePendingMarks are the re-check times of a pending draft after its
// ingest (#506 C9.2); after the last one, the hourly sweep takes over.
var provenancePendingMarks = []time.Duration{2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 60 * time.Minute}

// provenancePendingDue reports whether a record is a draft awaiting its push
// (not on the trusted branch yet, or different from it), younger than 24
// hours, with a re-check mark passed since it was last checked.
func provenancePendingDue(rec *persistence.ChunkProvenance, now time.Time) bool {
	if rec.State == ProvenanceVerified || (rec.Reason != ProvReasonNotFound && rec.Reason != ProvReasonBlobMismatch) {
		return false
	}
	if rec.CreatedAt.IsZero() || now.Sub(rec.CreatedAt) > 24*time.Hour {
		return false
	}
	for _, m := range provenancePendingMarks {
		mark := rec.CreatedAt.Add(m)
		if !now.Before(mark) && rec.CheckedAt.Before(mark) {
			return true
		}
	}
	return false
}

// ProvenanceSweepResult counts one project's pass. Examined is the
// denominator: every record read.
type ProvenanceSweepResult struct {
	Examined  int
	Terminal  int
	Unchanged int
	Changed   int
	Event     *persistence.MemoryVerificationEvent
}

const provenanceSweepPage = 200

// SweepProject re-checks one project's records. Records whose state changes
// are applied as ONE audited event, chunks and records together; a record
// that only moves its checked_at is updated alone.
func (s *ProvenanceSweeper) SweepProject(ctx context.Context, projectID string) (ProvenanceSweepResult, error) {
	return s.sweep(ctx, projectID, nil)
}

// SweepPending re-checks only the drafts that are due on the short schedule
// (#506 C9.2), in every project.
func (s *ProvenanceSweeper) SweepPending(ctx context.Context) {
	projects, err := s.Store.ListProvenanceProjects(ctx)
	if err != nil {
		return
	}
	now := s.Verifier.now()
	for _, p := range projects {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.sweep(ctx, p, func(rec *persistence.ChunkProvenance) bool {
			return provenancePendingDue(rec, now)
		}); err != nil {
			s.Logger.Warn().Err(err).Str("project_id", p).Msg("provenance sweep: pending pass failed")
		}
	}
}

// firstSweep reports, once per project per process, that this is its first
// sweep: the one that heals records written before #506 slice 1.
func (s *ProvenanceSweeper) firstSweep(projectID string) bool {
	s.healMu.Lock()
	defer s.healMu.Unlock()
	if s.healed == nil {
		s.healed = map[string]bool{}
	}
	if s.healed[projectID] {
		return false
	}
	s.healed[projectID] = true
	return true
}

func (s *ProvenanceSweeper) sweep(ctx context.Context, projectID string, only func(*persistence.ChunkProvenance) bool) (ProvenanceSweepResult, error) {
	var out ProvenanceSweepResult
	var changes []persistence.ArtifactVerificationChange
	heal := only == nil && s.firstSweep(projectID)
	verifiedDocs := map[provDoc]bool{}
	after := ""
	for {
		page, err := s.Store.ListProvenance(ctx, projectID, after, provenanceSweepPage)
		if err != nil {
			return out, err
		}
		for _, rec := range page {
			after = rec.ArtifactID
			if only != nil && !only(rec) {
				continue
			}
			out.Examined++
			if ProvenanceTerminal(rec.State, rec.Reason) {
				out.Terminal++
				continue
			}
			o, err := s.recheckOne(ctx, rec, heal)
			if err != nil {
				return out, err
			}
			if o.verifiedDoc {
				verifiedDocs[provDoc{rec.RepoScope, rec.DocumentPath}] = true
			}
			if o.change != nil {
				changes = append(changes, *o.change)
			} else {
				out.Unchanged++
			}
		}
		if len(page) < provenanceSweepPage {
			break
		}
	}
	out.Changed = len(changes)
	if len(changes) == 0 {
		s.supersedeVerified(ctx, projectID, verifiedDocs)
		return out, nil
	}
	ev := &persistence.MemoryVerificationEvent{
		ProjectID: projectID, Kind: persistence.MemoryVerificationKindSweep, Actor: ProvenanceSweepActor,
		Reason: "provenance re-check", Examined: out.Examined, Unchanged: out.Unchanged,
		// Terminal records are skipped, not re-checked (code review 9a9a F5).
		Skipped: out.Terminal,
	}
	if err := s.Store.ApplyVerificationChange(ctx, ev, changes); err != nil {
		return out, err
	}
	out.Event = ev
	s.logHeal(projectID, ev, changes)
	s.supersedeVerified(ctx, projectID, verifiedDocs)
	return out, nil
}

// recheckOutcome is one record's sweep result: a change to apply, or none
// (the record was re-stamped in place), and whether the document is
// provenance-verified after it.
type recheckOutcome struct {
	change      *persistence.ArtifactVerificationChange
	verifiedDoc bool
}

// recheckOne re-runs the rule for one stored record.
func (s *ProvenanceSweeper) recheckOne(ctx context.Context, rec *persistence.ChunkProvenance, heal bool) (recheckOutcome, error) {
	res := s.Verifier.Recheck(ctx, ProvenancePrior{
		ProjectID: rec.ProjectID, RepoScope: rec.RepoScope, Path: rec.DocumentPath, State: rec.State,
		Reason: rec.Reason, CommitSHA: rec.CommitSHA, ContentBlobSHA: rec.ContentBlobSHA,
	})
	if !res.Verified() && ProvenanceUnexaminable(res.Reason) {
		return s.recheckUnexaminable(ctx, rec, res, heal)
	}
	next := ProvenanceRecord(rec.ProjectID, rec.ArtifactID, rec.RepoScope, res)
	next.CreatedAt = rec.CreatedAt
	if res.Verified() && rec.State == ProvenanceVerified && rec.VerifiedAt != nil {
		next.VerifiedAt = rec.VerifiedAt
	}
	o := recheckOutcome{verifiedDoc: next.State == ProvenanceVerified}
	if next.State == rec.State && next.Reason == rec.Reason && next.CommitSHA == rec.CommitSHA &&
		next.OriginBlobSHA == rec.OriginBlobSHA {
		return o, s.Store.UpsertProvenance(ctx, next)
	}
	ch := persistence.ArtifactVerificationChange{ArtifactID: rec.ArtifactID, Provenance: next}
	if next.State == ProvenanceVerified {
		ch.NewStatus, ch.NewValidatorRole = "verified", ValidatorRoleProvenance
	} else {
		// Only the stamps the verifier made are withdrawn: the provenance
		// stamp, or (#506) the unavailable stamp of a record that could not
		// be examined before.
		ch.NewStatus, ch.OnlyValidatorRole = "unverified", ValidatorRoleProvenance
		if ProvenanceUnexaminable(rec.Reason) {
			ch.OnlyValidatorRole = ValidatorRoleProvenanceUnavailable
		}
	}
	o.change = &ch
	return o, nil
}

// recheckUnexaminable handles a re-check that could not examine (#506 C4/F4):
// it changes nothing. A verified record keeps its stamp (a missing clone never
// empties the guard's protected set), an examined failure stays unverified,
// an unavailable one stays unavailable. On the first sweep after start, an
// unavailable record's chunks are stamped, for records written before #506
// slice 1 stored them as before #482 (the store skips chunks already stamped).
func (s *ProvenanceSweeper) recheckUnexaminable(ctx context.Context, rec *persistence.ChunkProvenance, res ProvenanceResult, heal bool) (recheckOutcome, error) {
	o := recheckOutcome{verifiedDoc: rec.State == ProvenanceVerified}
	kept := *rec
	kept.CheckedAt = res.CheckedAt
	if heal && ProvenanceUnexaminable(rec.Reason) {
		o.change = &persistence.ArtifactVerificationChange{ArtifactID: rec.ArtifactID,
			Provenance: &kept, NewStatus: "verified", NewValidatorRole: ValidatorRoleProvenanceUnavailable,
			// Never a chunk a person or an event demoted (security review
			// 2026-10-23): the heal is for records written before slice 1.
			OnlyNeverStamped: true}
		return o, nil
	}
	return o, s.Store.UpsertProvenance(ctx, &kept)
}

// logHeal says how much the post-upgrade heal widened trust (code review 5420
// F4): the records whose never-stamped chunks became provenance:unavailable.
func (s *ProvenanceSweeper) logHeal(projectID string, ev *persistence.MemoryVerificationEvent, changes []persistence.ArtifactVerificationChange) {
	healed := 0
	for _, ch := range changes {
		if ch.OnlyNeverStamped {
			healed++
		}
	}
	if healed > 0 {
		s.Logger.Info().Str("project_id", projectID).Int("records", healed).Str("event_id", ev.ID).
			Msg("provenance sweep: stamped never-stamped chunks of records that could not be examined as provenance:unavailable (trusted as before #482)")
	}
}

// provDoc is one document: its repository scope and path.
type provDoc struct{ scope, path string }

// supersedeVerified runs document supersession for each provenance-verified
// document after the stamps are written (#506 C9.1).
func (s *ProvenanceSweeper) supersedeVerified(ctx context.Context, projectID string, docs map[provDoc]bool) {
	if s.Supersede == nil {
		return
	}
	for d := range docs {
		if d.scope == "" || d.path == "" {
			continue
		}
		if _, err := s.Supersede(ctx, projectID, d.scope, d.path); err != nil {
			s.Logger.Warn().Err(err).Str("project_id", projectID).Str("path", d.path).
				Msg("provenance sweep: supersession after verification failed; the next pass retries")
		}
	}
}

// SweepAll runs SweepProject for every project with a record. One project's
// failure is logged and does not stop the others.
func (s *ProvenanceSweeper) SweepAll(ctx context.Context) {
	s.FetchClones(ctx)
	projects, err := s.Store.ListProvenanceProjects(ctx)
	if err != nil {
		s.Logger.Warn().Err(err).Msg("provenance sweep: list projects failed")
		return
	}
	for _, p := range projects {
		if ctx.Err() != nil {
			return
		}
		res, err := s.SweepProject(ctx, p)
		if err != nil {
			s.Logger.Warn().Err(err).Str("project_id", p).Msg("provenance sweep: project failed")
			continue
		}
		ev := s.Logger.Debug()
		if res.Changed > 0 {
			ev = s.Logger.Info()
		}
		ev.Str("project_id", p).Int("examined", res.Examined).Int("terminal", res.Terminal).
			Int("unchanged", res.Unchanged).Int("changed", res.Changed).Msg("provenance sweep: project re-checked")
	}
}

// Run drives the periodic loop until ctx is cancelled. The first pass runs
// one interval after start.
func (s *ProvenanceSweeper) Run(ctx context.Context) {
	if s == nil || s.Store == nil || s.Verifier == nil {
		return
	}
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultProvenanceSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	pendingEvery := s.PendingInterval
	if pendingEvery <= 0 {
		pendingEvery = DefaultProvenancePendingInterval
	}
	pending := time.NewTicker(pendingEvery)
	defer pending.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.LeaderGate != nil && !s.LeaderGate.IsLeader() {
				continue
			}
			s.SweepAll(ctx)
		case <-pending.C:
			if s.LeaderGate != nil && !s.LeaderGate.IsLeader() {
				continue
			}
			s.SweepPending(ctx)
		}
	}
}
