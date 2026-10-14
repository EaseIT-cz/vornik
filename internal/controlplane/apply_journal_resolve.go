package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// The resolve verb (config-apply journal design §12, GitHub #285): the one
// supported way out of a DRIFT row, which blocks its affected projects
// (§10.4) until an operator decides.

// Resolve modes.
const (
	// ResolveKeep accepts the files as they are; no file is written.
	ResolveKeep = "keep"
	// ResolveRestore writes the recorded pre-images back.
	ResolveRestore = "restore"
)

var (
	// ErrJournalNotDrift means resolve refused a row that is not DRIFT (an open
	// row in another state belongs to the reconciler; a terminal row that was
	// never resolved is not the operator's to change).
	ErrJournalNotDrift = errors.New("control-plane: journal row is not in DRIFT")
	// ErrResolveMode means the mode is neither keep nor restore.
	ErrResolveMode = errors.New("control-plane: resolve mode must be keep or restore")
)

// Resolve outcomes (§12.5.2): what a resolve decided, in one word.
const (
	// ResolveOutcomeApplied: every target intended, the proposal APPROVED;
	// the row and the proposal are APPLIED together.
	ResolveOutcomeApplied = "applied"
	// ResolveOutcomeAlreadyApplied: every target intended and the proposal
	// already APPLIED; the row closes APPLIED and the proposal is not written.
	ResolveOutcomeAlreadyApplied = "already_applied"
	// ResolveOutcomeKeptUnapproved: every target intended but the proposal is
	// not APPROVED (REJECTED, DRAFT, ...); the operator kept the files, the
	// row closes FAILED with a note and the proposal is not written (#301).
	ResolveOutcomeKeptUnapproved = "kept_unapproved"
	// ResolveOutcomeKeptHandEdit: a target was edited by hand; the row closes
	// FAILED and an APPROVED proposal is retired.
	ResolveOutcomeKeptHandEdit = "kept_hand_edit"
	// ResolveOutcomeRestored: the pre-images were written back.
	ResolveOutcomeRestored = "restored"
)

// Refusal codes (§12.5.3): every case where resolve declines to act. The
// door answers each 409 with the remedy.
const (
	RefusalNotDrift               = "JOURNAL_NOT_DRIFT"
	RefusalApplyInProgress        = "APPLY_IN_PROGRESS"
	RefusalWriterFenced           = "WRITER_FENCED"
	RefusalRootMismatch           = "JOURNAL_ROOT_MISMATCH"
	RefusalProposalMissing        = "PROPOSAL_MISSING"
	RefusalProposalContentChanged = "PROPOSAL_CONTENT_CHANGED"
	RefusalProposalChanged        = "PROPOSAL_CHANGED"
	// RefusalRestoreApplied: --restore on a row whose proposal the ledger
	// already holds as APPLIED (§12.5.6, audit 2026-10-12 finding B).
	RefusalRestoreApplied = "PROPOSAL_ALREADY_APPLIED"
)

// ResolveRefusal is resolve declining to act, with what to do about it. It
// wraps the underlying sentinel, so errors.Is still matches it.
type ResolveRefusal struct {
	Code   string
	Reason string
	Remedy string
	Err    error
}

func (r *ResolveRefusal) Error() string { return r.Reason + ". Remedy: " + r.Remedy }

func (r *ResolveRefusal) Unwrap() error { return r.Err }

// ResolveResult is what a resolve did, for the audit row and the caller.
type ResolveResult struct {
	JournalID        string   `json:"journalId"`
	ProposalID       string   `json:"proposalId"`
	Mode             string   `json:"mode"`
	State            string   `json:"state"`          // the row's state after the call
	ProposalStatus   string   `json:"proposalStatus"` // the proposal's status after the call
	Changed          bool     `json:"changed"`        // false: already resolved in this mode
	AffectedProjects []string `json:"affectedProjects"`
	LeftInPlace      []string `json:"leftInPlace,omitempty"` // created files that drifted (restore)
	ReloadError      string   `json:"reloadError,omitempty"`
	// Outcome is one of the ResolveOutcome* words; empty on an idempotent
	// repeat (changed: false). Message says the same in one plain sentence.
	Outcome string `json:"outcome,omitempty"`
	Message string `json:"message,omitempty"`
	// ProposalStatusBefore is the proposal's status as read under the lock;
	// Note is the row's failure column as this resolve wrote it.
	ProposalStatusBefore string `json:"proposalStatusBefore,omitempty"`
	Note                 string `json:"note,omitempty"`
}

// resolvedNote is the failure-column prefix that marks a row resolved; it
// is also the idempotency marker.
func resolvedNote(mode, actor string) string {
	return "resolved " + mode + " by " + actor + ": "
}

func resolvedMode(failure string) string {
	for _, m := range []string{ResolveKeep, ResolveRestore} {
		if strings.HasPrefix(failure, "resolved "+m+" by ") {
			return m
		}
	}
	return ""
}

// ListOpenJournal returns every open journal row, for `journal list`.
func (e *ApplyEngine) ListOpenJournal(ctx context.Context) ([]*persistence.ConfigApplyJournal, error) {
	if e.Journal == nil {
		return nil, errors.New("control-plane: the apply journal is not wired")
	}
	return e.Journal.ListOpen(ctx)
}

// ResolveJournal resolves one DRIFT row (§12.2, §12.5). Every case where it
// declines to act is a *ResolveRefusal carrying a remedy (§12.5.3). After a
// result (the idempotent repeat included) it runs OnResolved, outside the
// locks, whichever door called it (§13.3, GitHub #302).
func (e *ApplyEngine) ResolveJournal(ctx context.Context, id, mode, actor string) (*ResolveResult, error) {
	res, err := e.resolveJournal(ctx, id, mode, actor)
	if err == nil && res != nil && e.OnResolved != nil {
		e.OnResolved(ctx, res.JournalID)
	}
	return res, err
}

func (e *ApplyEngine) resolveJournal(ctx context.Context, id, mode, actor string) (*ResolveResult, error) {
	if mode != ResolveKeep && mode != ResolveRestore {
		return nil, ErrResolveMode
	}
	if e.Journal == nil {
		return nil, errors.New("control-plane: the apply journal is not wired")
	}
	// The API passes the caller's principal when no author was given; this
	// default is only for direct callers (tests, a future in-process door).
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "operator"
	}
	if !configWriteMu.TryLock() {
		return nil, refuseBusy()
	}
	defer configWriteMu.Unlock()
	if !e.mu.TryLock() {
		return nil, refuseBusy()
	}
	defer e.mu.Unlock()

	row, err := e.Journal.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	res := &ResolveResult{JournalID: row.ID, ProposalID: row.ProposalID, Mode: mode, State: row.State, AffectedProjects: row.AffectedProjects}
	if row.State != persistence.JournalStateDrift {
		done := resolvedMode(row.Failure)
		if row.TerminalAt != nil && done == mode {
			res.ProposalStatus = e.proposalStatus(ctx, row.ProposalID)
			return res, nil // idempotent: already resolved this way
		}
		return nil, &ResolveRefusal{Code: RefusalNotDrift, Err: ErrJournalNotDrift,
			Reason: fmt.Sprintf("journal %s is %s, not DRIFT", row.ID, row.State),
			Remedy: "vornikctl control-plane journal list shows the open rows; an open row in another state is finished by " +
				"the reconciler at the next daemon start, and a row already resolved the other way stays as it was resolved"}
	}
	// Both modes write (files or the ledger): a fenced process writes
	// neither (§1.2, §10.1).
	if isFenced, reason := e.fenced(ctx); isFenced {
		return nil, &ResolveRefusal{Code: RefusalWriterFenced, Err: ErrWriterFenced,
			Reason: "this daemon does not hold the config writer lease: " + reason,
			Remedy: "run the verb against the daemon that holds the config writer lease"}
	}
	resolved, p, err := e.resolvedFromJournal(ctx, row)
	if err != nil {
		return nil, resolveSetupRefusal(row, err)
	}
	res.ProposalStatusBefore = p.Status
	note := resolvedNote(mode, actor) + row.Failure
	switch {
	case mode == ResolveKeep:
		err = e.resolveKeep(ctx, row, p, resolved, actor, note, res)
	case p.Status == persistence.ProposalStatusApplied:
		// The ledger holds this proposal as APPLIED; writing the pre-images
		// back and closing REVERTED would leave the row and the ledger in
		// disagreement (§3). APPLIED leaves only by a rollback, which needs
		// the engine lock held here, so the status read is stable (§12.5.6,
		// audit 2026-10-12 finding B).
		return nil, &ResolveRefusal{Code: RefusalRestoreApplied,
			Reason: fmt.Sprintf("journal %s's proposal %s is already APPLIED in the ledger, so restoring its pre-images "+
				"would leave the files and the ledger disagreeing", row.ID, p.ID),
			Remedy: "resolve with --keep (the row closes APPLIED, as the ledger already says), then, to undo the change, " +
				"vornikctl control-plane rollback " + p.ID + ", which restores the pre-apply snapshot and records ROLLED_BACK"}
	default:
		err = e.resolveRestore(ctx, row, resolved, note, res)
	}
	if err != nil {
		return nil, err
	}
	res.Changed = true
	res.ProposalStatus = e.proposalStatus(ctx, row.ProposalID)
	e.Logger.Info().Str("journal_id", row.ID).Str("proposal_id", row.ProposalID).Str("mode", mode).
		Str("actor", actor).Str("state", res.State).Str("outcome", res.Outcome).Str("proposal_status", res.ProposalStatus).
		Strs("unblocked_projects", row.AffectedProjects).
		Msg("control-plane: DRIFT journal row resolved; its projects are no longer blocked")
	return res, nil
}

func refuseBusy() error {
	return &ResolveRefusal{Code: RefusalApplyInProgress, Err: ErrApplyInProgress,
		Reason: "an apply or reconcile is running", Remedy: "retry when it finishes"}
}

// resolveSetupRefusal turns the reasons resolvedFromJournal cannot pair the
// row with its proposal into refusals (§12.5.3). A missing proposal does not
// keep wrapping ErrNotFound: the door answers that 404 "no journal row".
func resolveSetupRefusal(row *persistence.ConfigApplyJournal, err error) error {
	const unhandled = "none in this verb (config-apply journal design §12.5.3); report it, naming the journal id"
	switch {
	case errors.Is(err, ErrJournalRootMismatch):
		return &ResolveRefusal{Code: RefusalRootMismatch, Err: err, Reason: err.Error(),
			Remedy: "resolve it on the daemon whose config directory is the row's deployment root (" + row.DeploymentRoot + ")"}
	case errors.Is(err, persistence.ErrNotFound):
		return &ResolveRefusal{Code: RefusalProposalMissing, Remedy: unhandled,
			Reason: fmt.Sprintf("journal %s's proposal %s no longer exists, so its files cannot be checked against it", row.ID, row.ProposalID)}
	case errors.Is(err, ErrJournalDigestMismatch):
		return &ResolveRefusal{Code: RefusalProposalContentChanged, Err: err, Reason: err.Error(), Remedy: unhandled}
	}
	return err
}

// resolveKeep accepts the files as they are; no file is written. What it
// records depends on the files and on the proposal's status as read under
// the lock (§12.5.2):
//
//   - every target intended, proposal APPROVED: the row and the proposal are
//     APPLIED together, in one ledger transaction;
//   - every target intended, proposal already APPLIED: the row closes
//     APPLIED and the proposal is not written;
//   - every target intended, proposal anything else (REJECTED, DRAFT, ...):
//     the operator kept the files of a change the ledger does not hold as
//     applied; the row closes FAILED with a note saying so and the proposal
//     is not written (GitHub #301);
//   - a target edited by hand: FAILED, and the proposal is retired only
//     when it is APPROVED.
func (e *ApplyEngine) resolveKeep(ctx context.Context, row *persistence.ConfigApplyJournal, p *persistence.ControlPlaneProposal,
	resolved []resolvedOp, actor, note string, res *ResolveResult) error {
	allIntended := true
	for _, ro := range resolved {
		st, err := targetStatus(ro)
		if err != nil {
			return err
		}
		if st != "intended" {
			allIntended = false
		}
	}
	status := p.Status
	switch {
	case allIntended && status == persistence.ProposalStatusApproved:
		if err := e.Journal.MarkAppliedWithLedger(ctx, persistence.JournalLedgerCommit{
			JournalID: row.ID, From: persistence.JournalStateDrift, To: persistence.JournalStateApplied,
			GenerationAfter: e.generation(), ProposalID: p.ID, AppliedBy: actor,
			Snapshot: buildSnapshot(p, resolved), Failure: note,
		}); err != nil {
			if errors.Is(err, persistence.ErrProposalNotApproved) {
				// The status moved between the read above and the commit (the
				// agent-admin service decides proposals without this lock); the
				// transaction rolled back and the row is still DRIFT.
				return &ResolveRefusal{Code: RefusalProposalChanged, Err: err,
					Reason: "proposal " + p.ID + " left APPROVED while the row was being resolved",
					Remedy: "retry; the next attempt reads its new status"}
			}
			if errors.Is(err, persistence.ErrInvalidTransition) {
				return rowMoved(row)
			}
			return fmt.Errorf("journal DRIFT→APPLIED: %w", err)
		}
		res.State, res.Outcome, res.Note = persistence.JournalStateApplied, ResolveOutcomeApplied, note
		res.Message = "Kept the files as they are. They hold what proposal " + p.ID +
			" wrote, so the row and the proposal are recorded as APPLIED."
		return nil
	case allIntended && status == persistence.ProposalStatusApplied:
		// The ledger already says APPLIED, so an APPLIED row keeps §3's
		// invariant; the proposal is not written.
		if err := e.closeDrift(ctx, row, persistence.JournalStateApplied, note); err != nil {
			return err
		}
		res.State, res.Outcome, res.Note = persistence.JournalStateApplied, ResolveOutcomeAlreadyApplied, note
		res.Message = "Kept the files as they are. They hold what proposal " + p.ID +
			" wrote, and that proposal is already APPLIED; the row is closed as APPLIED and the proposal is unchanged."
		return nil
	case allIntended:
		// GitHub #301: the §12.2 marker first (idempotency), then what the
		// operator decided, then the reason the row parked.
		note = resolvedNote(ResolveKeep, actor) + "operator kept files of a " + status + " proposal; the proposal stays " +
			status + "; " + row.Failure
		if err := e.closeDrift(ctx, row, persistence.JournalStateFailed, note); err != nil {
			return err
		}
		res.State, res.Outcome, res.Note = persistence.JournalStateFailed, ResolveOutcomeKeptUnapproved, note
		res.Message = "Kept the files as they are. They hold what proposal " + p.ID + " wrote, but that proposal is " +
			status + ", so the change is not recorded as applied and the proposal stays " + status +
			"; the row is closed as FAILED (resolved, not an error)."
		return nil
	}
	note += "; a target was edited by hand, so the proposal is not applied"
	if status != persistence.ProposalStatusApproved {
		note += "; the proposal stays " + status
	}
	if err := e.closeDrift(ctx, row, persistence.JournalStateFailed, note); err != nil {
		return err
	}
	res.State, res.Outcome, res.Note = persistence.JournalStateFailed, ResolveOutcomeKeptHandEdit, note
	if status == persistence.ProposalStatusApproved {
		e.autoRetireStale(ctx, row.ProposalID)
		res.Message = "Kept the files as they are. A file was edited by hand, so proposal " + p.ID +
			" is not recorded as applied and is retired; the row is closed as FAILED (resolved, not an error)."
		return nil
	}
	res.Message = "Kept the files as they are. A file was edited by hand, so proposal " + p.ID +
		" is not recorded as applied; it stays " + status + "; the row is closed as FAILED (resolved, not an error)."
	return nil
}

// closeDrift moves a DRIFT row to a terminal state without touching the
// proposal. A row that left DRIFT meanwhile is a refusal, not a 500.
func (e *ApplyEngine) closeDrift(ctx context.Context, row *persistence.ConfigApplyJournal, to, note string) error {
	err := e.Journal.Transition(ctx, row.ID, persistence.JournalStateDrift, to,
		persistence.JournalPatch{Failure: note, GenerationAfter: e.generation(), Terminal: true})
	if errors.Is(err, persistence.ErrInvalidTransition) {
		return rowMoved(row)
	}
	if err != nil {
		return fmt.Errorf("journal DRIFT→%s: %w", to, err)
	}
	return nil
}

func rowMoved(row *persistence.ConfigApplyJournal) error {
	return &ResolveRefusal{Code: RefusalNotDrift, Err: ErrJournalNotDrift,
		Reason: "journal " + row.ID + " left DRIFT while it was being resolved",
		Remedy: "retry; the next attempt reads its new state"}
}

// resolveRestore writes every recorded pre-image back. A target that
// existed gets its pre-image whatever it holds now (the explicit flag is
// the operator's consent to overwrite a hand edit, §5); a target this row
// created is deleted only when it still holds exactly this row's content
// (§6), otherwise it is left and named. Then Reload, and DRIFT → REVERTED;
// the proposal is not written and stays in its status (APPROVED, or any
// status the ledger does not hold as applied). An APPLIED proposal never
// reaches here: ResolveJournal refuses it (§12.5.6).
func (e *ApplyEngine) resolveRestore(ctx context.Context, row *persistence.ConfigApplyJournal, resolved []resolvedOp, note string, res *ResolveResult) error {
	// Reverse op order, as revertJournaled: each target is decided on its
	// own, so the order matters only for symmetry with the forward apply.
	for i := len(resolved) - 1; i >= 0; i-- {
		ro := resolved[i]
		cur, existed, err := readIfExists(ro.target)
		if err != nil {
			return fmt.Errorf("restore: read %s: %w", ro.op.Path, err)
		}
		switch {
		case ro.existed:
			// Already at the pre-image (never written, or edited back by
			// hand): nothing to write. A hand-deleted file is recreated.
			if existed && hashBytes(cur) == hashBytes(ro.preImage) {
				continue
			}
			if werr := atomicWrite(ro.target, ro.preImage); werr != nil {
				return fmt.Errorf("restore %s: %w", ro.op.Path, werr)
			}
		case !existed:
			continue
		case atIntended(ro.op, cur, existed):
			if rerr := os.Remove(ro.target); rerr != nil && !os.IsNotExist(rerr) {
				return fmt.Errorf("restore: delete created %s: %w", ro.op.Path, rerr)
			}
		default:
			res.LeftInPlace = append(res.LeftInPlace, ro.op.Path)
		}
	}
	if rerr := e.reload(); rerr != nil {
		res.ReloadError = rerr.Error()
		e.Logger.Error().Err(rerr).Str("journal_id", row.ID).Msg("control-plane: reload after a journal restore failed; the pre-images are on disk")
	}
	if len(res.LeftInPlace) > 0 {
		note += "; created files left in place: " + strings.Join(res.LeftInPlace, ", ")
	}
	if err := e.closeDrift(ctx, row, persistence.JournalStateReverted, note); err != nil {
		return err
	}
	res.State, res.Outcome, res.Note = persistence.JournalStateReverted, ResolveOutcomeRestored, note
	res.Message = "Restored the content the files had before the apply; the row is closed as REVERTED and proposal " +
		row.ProposalID + " is left as it was."
	return nil
}

func (e *ApplyEngine) proposalStatus(ctx context.Context, id string) string {
	p, err := e.Proposals.GetByID(ctx, id)
	if err != nil {
		return ""
	}
	return p.Status
}
