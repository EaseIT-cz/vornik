// Package workflowapply applies and rolls back workflow proposals: it writes
// a proposal's genome to the deployed workflow file, records the file it
// replaced, triggers a config reload, and restores that file on rollback.
//
// It lived in internal/memetic until 2026-09-16, because the memetic
// architect was its first caller. The architect was replaced as the healing
// candidate producer (config-assistant design §6.3.4b) and deleted; this
// machinery was never architect-specific and moved here unchanged rather than
// going with it. Its callers now are the config assistant's healing
// candidates, the deterministic recipe path, and the operator's apply and
// rollback buttons.
//
// Until 2026-09-26 an apply also committed the file to the operator's git
// checkout and a rollback ran `git revert`. Both are gone: the apply and
// rollback endpoints are requests, and the process-spawn law
// (https://docs.vornik.io, S3) forbids a
// request from making the daemon run a program. The history lives on the
// proposal row instead: the pre-apply file for rollback, the ledger for audit.
package workflowapply

// Apply path for approved proposals. Reads the approved proposal, records the
// deployed file as it is, writes the new YAML to both config trees (source +
// deployed, per the two-trees discipline), refreshes the daemon's in-memory
// config (which fires the cross-instance NOTIFY), and stamps the proposal row
// as applied together with the recorded file.
//
// Apply runs ONLY against status=approved rows. Pending rows must be decided
// first via the operator review path. The MarkApplied repository contract
// already enforces the approved → applied transition at the SQL layer; the
// applier also short-circuits before any filesystem work for clarity in the
// error message.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// NoGitCommit is what applied_commit and rollback_commit record: the daemon
// makes no commits (process-spawn law, S3). Rows written before 2026-09-26 may
// carry a real SHA instead, which rollback names when it cannot restore them.
const NoGitCommit = "no-git"

// ErrProposalNotApproved fires when the operator tries to apply a
// proposal that isn't in status=approved. Maps to 409 Conflict at
// the API layer. Distinct from ErrInvalidProposalTransition so
// the admin endpoint can return a clear message.
var ErrProposalNotApproved = errors.New("memetic: proposal must be approved before apply")

// WorkflowWriter writes a workflow YAML body to both config trees
// (source + deployed). Adapters in the service layer handle the actual
// filesystem semantics.
type WorkflowWriter interface {
	// Write places `body` at workflows/<workflowID>.md in both
	// trees. It returns the absolute source-tree path, or "" when there
	// is no source tree (deployed-only deployment). Nothing commits it.
	Write(ctx context.Context, workflowID string, body []byte) (sourcePath string, err error)
}

// DeployedReader is the WorkflowWriter extension that returns the deployed
// workflow file as it is now. The applier reads it BEFORE Write to record the
// pre-apply file, which is both config_template_drift's evidence (slice E)
// and the rollback record (process-spawn law S3).
type DeployedReader interface {
	ReadDeployed(ctx context.Context, workflowID string) ([]byte, error)
}

// preApplyStamper is the repository extension that marks a row applied and
// records its pre-apply file in one UPDATE, so the two cannot disagree.
type preApplyStamper interface {
	MarkAppliedWithPreApply(ctx context.Context, id, appliedCommit string, preApply []byte) error
}

// ConfigReloadTrigger triggers the daemon's in-process config
// reload. The existing service.ConfigReloader satisfies this via
// a one-line adapter; the post-reload hook fires the cross-
// instance NOTIFY so peer replicas refresh too (Slice 3a of the
// horizontal-scaling design). nil means single-replica deployment
// without a reloader wired — the applier logs and continues; the
// next file-watcher tick or explicit reload picks up the file.
type ConfigReloadTrigger interface {
	Reload() error
}

// ApplierConfig tunes the applier's behaviour.
type ApplierConfig struct {
	// Logger receives the applier's warnings; nil discards them.
	Logger *zerolog.Logger
}

// Applier owns the apply-path workflow. Constructed via NewApplier
// with the narrow interfaces wired by the service layer. The proposals
// repo and the writer are required; the reloader is nil-safe.
type Applier struct {
	proposals persistence.WorkflowProposalRepository
	writer    WorkflowWriter
	reloader  ConfigReloadTrigger
	cfg       ApplierConfig
}

// NewApplier wires the applier.
func NewApplier(
	proposals persistence.WorkflowProposalRepository,
	writer WorkflowWriter,
	reloader ConfigReloadTrigger,
	cfg ApplierConfig,
) *Applier {
	return &Applier{
		proposals: proposals,
		writer:    writer,
		reloader:  reloader,
		cfg:       cfg,
	}
}

// ErrNoRollbackRecord refuses an apply whose pre-apply file cannot be recorded:
// rollback restores that file, so without it the apply could not be undone.
var ErrNoRollbackRecord = errors.New("memetic: cannot record the deployed file before applying, so the apply could not be rolled back")

// Apply runs one apply turn for `proposalID`. Returns the updated
// proposal row on success. Error sentinels the API layer maps:
//
//   - ErrProposalNotApproved          → 409 (row isn't approved)
//   - persistence.ErrNotFound          → 404
//   - persistence.ErrInvalidProposalTransition → 409 (race; repo guard)
//   - ErrNoRollbackRecord             → the pre-apply file could not be read
//     or recorded; nothing was written
//
// On a filesystem failure the proposal row stays in status=approved so the
// operator can retry — the apply is idempotent at the row level even though it
// isn't fully transactional at the filesystem level.
func (a *Applier) Apply(ctx context.Context, proposalID, decidedBy string) (*persistence.WorkflowProposal, error) {
	if proposalID == "" {
		return nil, fmt.Errorf("memetic.Apply: proposalID is required")
	}
	if a.proposals == nil {
		return nil, fmt.Errorf("memetic.Apply: proposals repo not wired")
	}
	if a.writer == nil {
		return nil, fmt.Errorf("memetic.Apply: workflow writer not wired")
	}

	got, err := a.proposals.Get(ctx, proposalID)
	if err != nil {
		return nil, err
	}
	if got.Status != persistence.WorkflowProposalStatusApproved {
		return nil, fmt.Errorf("%w: current status=%s",
			ErrProposalNotApproved, got.Status)
	}

	// 0. The pre-apply file, read BEFORE the write (reading after it would
	// record the new genome). Required: it is the rollback record.
	preApply, stamper, err := a.capturePreApply(ctx, got)
	if err != nil {
		return nil, err
	}

	// 1. Filesystem writeback (both trees).
	if _, err := a.writer.Write(ctx, got.WorkflowID, []byte(got.ProposalYAML)); err != nil {
		return nil, fmt.Errorf("memetic.Apply: write workflow %q: %w", got.WorkflowID, err)
	}

	// 2. Config reload — refresh the local daemon's in-memory
	// registry AND broadcast NOTIFY to peer replicas via the
	// post-reload hook. Best-effort: a reload failure doesn't
	// undo the filesystem write; the file-watcher catches the
	// change within ~5s anyway.
	if a.reloader != nil {
		if err := a.reloader.Reload(); err != nil {
			_ = err // intentionally swallowed; see comment above
		}
	}

	// 3. Stamp the proposal row as applied, with the recorded file.
	if err := stamper.MarkAppliedWithPreApply(ctx, proposalID, NoGitCommit, preApply); err != nil {
		return nil, fmt.Errorf("memetic.Apply: mark applied: %w", err)
	}
	// Read the updated row back so the caller sees applied_at +
	// applied_commit. Best-effort; on read failure the operator
	// still gets confirmation that Apply succeeded (the row is
	// in the right state) but loses the in-line timestamps.
	updated, err := a.proposals.Get(ctx, proposalID)
	if err != nil {
		now := time.Now().UTC()
		return &persistence.WorkflowProposal{
			ID:            proposalID,
			WorkflowID:    got.WorkflowID,
			Status:        persistence.WorkflowProposalStatusApplied,
			AppliedAt:     &now,
			AppliedCommit: NoGitCommit,
		}, nil
	}
	return updated, nil
}

// capturePreApply reads the deployed file and returns it with the stamp that
// records it. Every refusal happens here, before anything is written: a writer
// that cannot read, a repository that cannot record, a failed read, or an
// empty file (an empty record reads as "not recorded", which rollback refuses).
// It assumes applies to one workflow are serialized (they are admin-driven);
// an apply interleaving between this read and the write would fold its change
// into this proposal's pre→post diff.
func (a *Applier) capturePreApply(ctx context.Context, p *persistence.WorkflowProposal) ([]byte, preApplyStamper, error) {
	reader, ok := a.writer.(DeployedReader)
	if !ok {
		return nil, nil, fmt.Errorf("%w: the workflow writer cannot read the deployed file", ErrNoRollbackRecord)
	}
	stamper, ok := a.proposals.(preApplyStamper)
	if !ok {
		return nil, nil, fmt.Errorf("%w: the proposal store cannot record it", ErrNoRollbackRecord)
	}
	body, err := reader.ReadDeployed(ctx, p.WorkflowID)
	if err != nil {
		if a.cfg.Logger != nil {
			a.cfg.Logger.Warn().Err(err).
				Str("event", "pre_apply_snapshot_missing").
				Str("proposal_id", p.ID).Str("workflow_id", p.WorkflowID).
				Msg("workflow apply: could not read the deployed file before writing; the apply is refused")
		}
		return nil, nil, fmt.Errorf("%w: read %q: %v", ErrNoRollbackRecord, p.WorkflowID, err)
	}
	if len(body) == 0 {
		return nil, nil, fmt.Errorf("%w: the deployed file %q is empty", ErrNoRollbackRecord, p.WorkflowID)
	}
	return body, stamper, nil
}
