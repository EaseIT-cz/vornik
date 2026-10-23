package persistence

import (
	"context"
	"time"
)

// Memory verification state (design 22, "Amendment 2026-10-21 (#482): a
// companion-delegated chunk is verified only by provenance"; migration 234).
//
// Three tables, both drivers:
//
//   - memory_chunk_provenance: what the provenance verifier saw for one
//     document version (one artifact), the evidence behind a
//     provenance-verified chunk;
//   - memory_verification_events: one row per bulk change of chunk
//     verification state (the operator backfill, a sweep pass, a restore),
//     with actor, reason and counts: the audited event;
//   - memory_verification_backup: each chunk such an event changed, with its
//     prior status and validator role, so the change is recoverable.

// Verification event kinds.
const (
	MemoryVerificationKindBackfill = "backfill"
	MemoryVerificationKindSweep    = "sweep"
	MemoryVerificationKindRestore  = "restore"
)

// ChunkProvenance is one provenance record.
type ChunkProvenance struct {
	ProjectID      string
	ArtifactID     string
	RepoScope      string
	DocumentPath   string
	CommitSHA      string
	ContentBlobSHA string
	OriginBlobSHA  string
	State          string // verified | stale | unverified
	Reason         string
	CheckedAt      time.Time
	VerifiedAt     *time.Time
	CreatedAt      time.Time
}

// MemoryVerificationEvent is one audited bulk change. Examined and Unchanged
// are the caller's denominator; Verified and Deverified are counted by the
// repository from the rows it actually changed. Skipped is set by a restore
// only (chunks changed since the restored event); an apply leaves it as the
// caller passed it.
type MemoryVerificationEvent struct {
	ID              string
	ProjectID       string
	Kind            string
	Actor           string
	Reason          string
	RestoresEventID string
	Examined        int
	Unchanged       int
	Verified        int
	Deverified      int
	Skipped         int
	CreatedAt       time.Time
}

// ArtifactVerificationChange sets the verification state of every live chunk
// of one artifact. NewStatus is "verified" or "unverified"; NewValidatorRole
// "" stores NULL. Provenance, when set, is upserted in the same transaction,
// before the chunks change. OnlyValidatorRole, when set, limits the change
// to chunks carrying that validator role (the sweep de-verifies only chunks
// it stamped).
type ArtifactVerificationChange struct {
	ArtifactID        string
	NewStatus         string
	NewValidatorRole  string
	OnlyValidatorRole string
	// OnlyNeverStamped narrows a "verified" change to chunks that were
	// never stamped: unverified, no validator role, and no backup row (no
	// verification event ever changed them). The #506 heal uses it so a
	// restart never re-verifies a chunk someone or something demoted.
	OnlyNeverStamped bool
	Provenance       *ChunkProvenance
}

// VerificationCandidate is an artifact with at least one chunk carrying the
// role-of-record stamp: validation_status 'verified' with validator_role
// equal to its producer_role, the producer an agent role.
type VerificationCandidate struct {
	ArtifactID   string
	TaskID       string
	ProducerRole string
	SourceName   string
	RepoScope    string
	Chunks       int
}

// VerificationChunk is one chunk of a candidate artifact, for the backfill's
// content sample.
type VerificationChunk struct {
	ID         string
	Content    string
	SourceName string
}

// MemoryVerificationRepository persists provenance records and audited
// verification changes. Implemented for both drivers; the shared contract
// is repotest.RunMemoryVerificationSuite.
type MemoryVerificationRepository interface {
	// UpsertProvenance writes the record for (project, artifact),
	// replacing any earlier one. CreatedAt is kept from the first write.
	UpsertProvenance(ctx context.Context, p *ChunkProvenance) error
	// GetProvenance returns the record, or nil when there is none.
	GetProvenance(ctx context.Context, projectID, artifactID string) (*ChunkProvenance, error)
	// ListProvenance pages one project's records in artifact_id order,
	// after afterArtifactID.
	ListProvenance(ctx context.Context, projectID, afterArtifactID string, limit int) ([]*ChunkProvenance, error)
	// ListProvenanceProjects returns every project with a record.
	ListProvenanceProjects(ctx context.Context) ([]string, error)
	// CompanionDelegatedTasks answers, for each task id, whether the task
	// or any task on its parent chain was created by a companion. A task
	// that cannot be found, an empty id, or a chain deeper than the cap is
	// reported true (fail closed).
	CompanionDelegatedTasks(ctx context.Context, taskIDs []string) (map[string]bool, error)
	// ListRoleOfRecordCandidates pages one project's candidate artifacts in
	// artifact_id order, after afterArtifactID.
	ListRoleOfRecordCandidates(ctx context.Context, projectID, afterArtifactID string, limit int) ([]VerificationCandidate, error)
	// ArtifactChunks returns up to limit live chunks of one artifact.
	ArtifactChunks(ctx context.Context, projectID, artifactID string, limit int) ([]VerificationChunk, error)
	// ApplyVerificationChange records ev (ID and CreatedAt filled when
	// empty), copies each affected chunk's prior state into the backup,
	// and applies the changes, all in one transaction. A chunk that is
	// superseded or refuted, or already in the target state, is not
	// touched. On return ev carries the counts the transaction applied.
	ApplyVerificationChange(ctx context.Context, ev *MemoryVerificationEvent, changes []ArtifactVerificationChange) error
	// RestoreVerificationEvent puts back the prior state of every chunk
	// event eventID changed, where the chunk is still in the state the
	// event left it in; others are skipped and counted. The restore is
	// itself an event (kind restore), returned.
	RestoreVerificationEvent(ctx context.Context, eventID, actor, reason string) (*MemoryVerificationEvent, error)
	// CountRestorableSelfCertified counts the chunks a restore of eventID
	// would put back to a role-of-record stamp (validator_role equal to
	// producer_role), for the restore's warning.
	CountRestorableSelfCertified(ctx context.Context, eventID string) (int, error)
	// GetVerificationEvent returns the event, or nil.
	GetVerificationEvent(ctx context.Context, eventID string) (*MemoryVerificationEvent, error)
}

// CompanionChainMaxDepth caps the parent walk of CompanionDelegatedTasks.
const CompanionChainMaxDepth = 64
