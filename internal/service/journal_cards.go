package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
)

// The approver-device card for an interrupted config apply (config-apply
// journal design §13.3, GitHub #302). A DRIFT row in config_apply_journal
// pauses its projects until an operator keeps or restores the files; before
// this only `vornikctl control-plane journal resolve` on the host could, and
// on 2026-10-08 an operator whose interface is Hermes had no way in. The
// daemon files one card per DRIFT row on the paired device; the decision runs
// the same engine call as the CLI, audited, with the device as principal.
// No agent can see (namespace ""), file or decide a card.

const (
	// journalCardTTL: an undecided card expires and is re-asked weekly.
	journalCardTTL = 7 * 24 * time.Hour
	// journalCardMaxGen bounds the re-asks (about seven months of weeks).
	journalCardMaxGen = 32
	// journalCardSweepEvery is the backstop sweep's period.
	journalCardSweepEvery = 5 * time.Minute
	// journalCardActor is the daemon actor on a withdrawal.
	journalCardActor = "system:config-journal"
	// journalCardPushSubject is the push subject for a card.
	journalCardPushSubject = "Vornik: a config change was interrupted"

	journalCardReasonResolved = "This config change was resolved on the Vornik host. Nothing needs deciding."
	journalCardReasonNotDrift = "This config change is no longer waiting for a decision. Nothing needs deciding."
	// journalCardRemedyNotDrift is the failed card's text when the row was
	// resolved the other way first (review 20261013-0a10 F2).
	journalCardRemedyNotDrift = "this change was already resolved, on the host or another device; nothing more to do"
)

// journalCardDoc is a card's rendered content: what the device shows and
// what its decision binds to (rendered_sha256).
type journalCardDoc struct {
	Kind       string   `json:"kind"`
	JournalID  string   `json:"journal_id"`
	ProposalID string   `json:"proposal_id"`
	Projects   []string `json:"projects"`
	Drift      string   `json:"drift"`
	Detail     string   `json:"detail"`
	Files      []string `json:"files"`
	Choices    []string `json:"choices"`
	// Applied: the proposal was APPLIED at filing, so restore is not
	// offered (§12.5.6).
	Applied bool `json:"applied"`
}

// journalCards files, withdraws and applies the cards.
type journalCards struct {
	devices   *approverdevice.Service
	repo      persistence.ApproverDeviceRepository
	journal   persistence.ApplyJournalRepository
	proposals persistence.ProposalRepository
	audit     persistence.AdminAuditRepository
	// resolve is the engine's ResolveJournal (a seam for tests).
	resolve func(ctx context.Context, id, mode, actor string) (*controlplane.ResolveResult, error)
	now     func() time.Time
	logger  zerolog.Logger
}

// journalCardID is the deterministic id of a row's gen-th card; the primary
// key is the cross-process guard for "one pending card per row".
func journalCardID(journalID string, gen int) string {
	sum := sha256.Sum256([]byte(persistence.ApprovalKindConfigJournalDrift + "\x00" + journalID + "\x00" + strconv.Itoa(gen)))
	return "apr_" + hex.EncodeToString(sum[:])[:16]
}

// Ensure files the row's card unless one is pending, awaiting its effect, or
// ended in a permanent failure. A row not in DRIFT gets none. Concurrent
// calls race read-then-file on purpose: the deterministic id's primary key
// lets exactly one insert win and the losers read it back (no lock needed).
func (j *journalCards) Ensure(ctx context.Context, journalID string) error {
	row, err := j.journal.Get(ctx, journalID)
	if err != nil {
		return err
	}
	if row.State != persistence.JournalStateDrift {
		return nil
	}
	for gen := 0; gen < journalCardMaxGen; gen++ {
		id := journalCardID(row.ID, gen)
		card, err := j.repo.GetRequest(ctx, id)
		if errors.Is(err, persistence.ErrNotFound) {
			return j.file(ctx, row, id)
		}
		if err != nil {
			return err
		}
		switch {
		case card.Status == persistence.ApprovalPending && j.now().Before(card.ExpiresAt):
			return nil // one exists
		case card.Status == persistence.ApprovalApproved && card.AppliedAt == nil:
			return nil // its effect is pending
		case card.Status == persistence.ApprovalApproved && card.ApplyError != "":
			return nil // the device told the operator to use the host
		case card.Status == persistence.ApprovalApproved:
			return nil // applied: the row is being resolved
		}
		// expired (or pending past its expiry), withdrawn, rejected: next gen.
	}
	j.logger.Warn().Str("journal_id", row.ID).Msg("config journal card: re-ask ceiling reached; resolve on the host")
	return nil
}

func (j *journalCards) file(ctx context.Context, row *persistence.ConfigApplyJournal, id string) error {
	doc := journalCardDoc{Kind: persistence.ApprovalKindConfigJournalDrift, JournalID: row.ID, ProposalID: row.ProposalID,
		Projects: append([]string{}, row.AffectedProjects...), Drift: controlplane.DriftClassOf(row.Failure), Detail: row.Failure,
		Choices: []string{controlplane.ResolveKeep, controlplane.ResolveRestore}}
	for _, op := range row.Ops {
		doc.Files = append(doc.Files, op.Path)
	}
	if j.proposals != nil {
		if p, err := j.proposals.GetByID(ctx, row.ProposalID); err == nil && p.Status == persistence.ProposalStatusApplied {
			doc.Applied, doc.Choices = true, []string{controlplane.ResolveKeep}
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	canon, err := approval.Canonical(raw)
	if err != nil {
		return err
	}
	sum, err := approval.CanonicalSHA256(canon)
	if err != nil {
		return err
	}
	now := j.now()
	r := persistence.AgentApprovalRequestRow{ID: id, Namespace: "", Kind: persistence.ApprovalKindConfigJournalDrift,
		Sentence: fmt.Sprintf("A config change for %s was interrupted. Keep the files as they are, or restore the previous version.", cardProjects(doc.Projects)),
		Rendered: canon, RenderedSHA256: sum, Status: persistence.ApprovalPending, CreatedAt: now, ExpiresAt: now.Add(journalCardTTL)}
	// CreateRequest, not CreateRequestCapped (review 9caf F6): the daemon's
	// cards are bounded by the DRIFT rows and must not crowd out enrollment.
	if err := j.devices.FileRequestPush(ctx, r, journalCardPushSubject); err != nil {
		if _, gerr := j.repo.GetRequest(ctx, id); gerr == nil {
			return nil // a concurrent filer won
		}
		return err
	}
	j.logger.Info().Str("journal_id", row.ID).Str("request_id", id).Msg("config journal card filed on the approver device")
	return nil
}

func cardProjects(p []string) string {
	for _, x := range p {
		if x == "*" {
			return "all projects"
		}
	}
	if len(p) == 0 {
		return "this installation"
	}
	return strings.Join(p, ", ")
}

// Withdraw withdraws every pending card of the row, auditing each one it
// withdrew. WithdrawTooLate and WithdrawAlready are success (the contract of
// §13.3); ErrNotWithdrawable is a wiring defect and logged as one.
func (j *journalCards) Withdraw(ctx context.Context, journalID, reason, why string) {
	for gen := 0; gen < journalCardMaxGen; gen++ {
		id := journalCardID(journalID, gen)
		card, err := j.repo.GetRequest(ctx, id)
		if errors.Is(err, persistence.ErrNotFound) {
			return
		}
		if err != nil {
			j.logger.Warn().Err(err).Str("journal_id", journalID).Msg("config journal card: read failed; the sweep retries")
			return
		}
		if card.Status != persistence.ApprovalPending {
			continue
		}
		res, err := j.devices.Withdraw(ctx, id, journalCardActor, reason)
		switch {
		case errors.Is(err, approverdevice.ErrNotWithdrawable):
			j.logger.Error().Str("request_id", id).Msg("config journal card: the kind is not registered withdrawable (wiring defect)")
			return
		case err != nil:
			j.logger.Warn().Err(err).Str("request_id", id).Msg("config journal card: withdraw failed; the sweep retries")
			continue
		}
		if res.Outcome == approverdevice.WithdrawDone {
			j.auditWithdrawn(ctx, id, journalID, why)
		}
	}
}

func (j *journalCards) auditWithdrawn(ctx context.Context, requestID, journalID, why string) {
	if j.audit == nil {
		return
	}
	raw, _ := json.Marshal(map[string]any{"journal_id": journalID, "reason": why})
	if err := j.audit.Insert(ctx, &persistence.AdminAuditEntry{Principal: "daemon", Source: "daemon",
		Action: "config_journal.card_withdrawn", Target: requestID, After: string(raw)}); err != nil {
		j.logger.Warn().Err(err).Str("request_id", requestID).Msg("config journal card: withdrawal audit write failed")
	}
}

// Sweep is the backstop (§13.3): every DRIFT row has a card, and every
// pending card whose row is no longer DRIFT is withdrawn.
func (j *journalCards) Sweep(ctx context.Context) {
	rows, err := j.journal.ListOpen(ctx)
	if err != nil {
		j.logger.Warn().Err(err).Msg("config journal card sweep: journal read failed")
		return
	}
	for _, row := range rows {
		if row.State == persistence.JournalStateDrift {
			if err := j.Ensure(ctx, row.ID); err != nil {
				j.logger.Warn().Err(err).Str("journal_id", row.ID).Msg("config journal card sweep: filing failed")
			}
		}
	}
	pending, err := j.repo.ListPending(ctx, j.now())
	if err != nil {
		j.logger.Warn().Err(err).Msg("config journal card sweep: pending read failed")
		return
	}
	for _, card := range pending {
		if card.Kind != persistence.ApprovalKindConfigJournalDrift {
			continue
		}
		var doc journalCardDoc
		if json.Unmarshal(card.Rendered, &doc) != nil || doc.JournalID == "" {
			continue
		}
		row, err := j.journal.Get(ctx, doc.JournalID)
		if err == nil && row.State == persistence.JournalStateDrift {
			continue
		}
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			continue
		}
		j.Withdraw(ctx, doc.JournalID, journalCardReasonNotDrift, "row no longer DRIFT")
	}
}

// Effect runs a decided card: the same engine call as the CLI, audited with
// the device as principal. A transient refusal is retried by the device
// tick; any other refusal fails the card with its remedy.
func (j *journalCards) Effect(ctx context.Context, r persistence.AgentApprovalRequestRow) error {
	var doc journalCardDoc
	if err := json.Unmarshal(r.Rendered, &doc); err != nil || doc.JournalID == "" {
		return fmt.Errorf("%w: invalid config journal card", approverdevice.ErrPermanent)
	}
	mode := r.DecidedChoice
	if mode != controlplane.ResolveKeep && mode != controlplane.ResolveRestore {
		return fmt.Errorf("%w: unknown choice %q", approverdevice.ErrPermanent, mode)
	}
	if j.resolve == nil {
		return errors.New("config journal card: the apply engine is not wired")
	}
	actor := "approver device " + r.DecidedByDevice
	res, err := j.resolve(ctx, doc.JournalID, mode, actor)
	j.auditResolve(ctx, r, doc.JournalID, mode, actor, res, err)
	return journalCardEffectError(err)
}

// journalCardEffectError maps a resolve's error to the device's retry rule.
func journalCardEffectError(err error) error {
	if err == nil {
		return nil
	}
	var refusal *controlplane.ResolveRefusal
	if !errors.As(err, &refusal) {
		return err // retried
	}
	switch refusal.Code {
	case controlplane.RefusalApplyInProgress, controlplane.RefusalWriterFenced:
		return err // transient: retried
	case controlplane.RefusalNotDrift:
		return fmt.Errorf("%w: %s", approverdevice.ErrPermanent, journalCardRemedyNotDrift)
	}
	return fmt.Errorf("%w: %s. Remedy: %s", approverdevice.ErrPermanent, refusal.Reason, refusal.Remedy)
}

func (j *journalCards) auditResolve(ctx context.Context, r persistence.AgentApprovalRequestRow, journalID, mode, actor string, res *controlplane.ResolveResult, rerr error) {
	if j.audit == nil {
		return
	}
	after := map[string]any{"mode": mode, "actor": actor, "request_id": r.ID}
	if res != nil {
		after["state"], after["proposal_status"], after["changed"] = res.State, res.ProposalStatus, res.Changed
		after["affected_projects"], after["left_in_place"] = res.AffectedProjects, res.LeftInPlace
		if res.Outcome != "" {
			after["outcome"] = res.Outcome
		}
	}
	if rerr != nil {
		after["refused"] = rerr.Error()
	}
	raw, _ := json.Marshal(after)
	if err := j.audit.Insert(ctx, &persistence.AdminAuditEntry{Principal: "approver_device:" + r.DecidedByDevice,
		Source: "approver_device", Action: "config_journal.resolve", Target: journalID, After: string(raw)}); err != nil {
		j.logger.Warn().Err(err).Str("journal_id", journalID).Msg("config journal card: resolve audit write failed; the resolution stands")
	}
}

// journalCardChoices is the page's choices, from the rendered card.
func journalCardChoices(r persistence.AgentApprovalRequestRow) []approverdevice.Choice {
	var doc journalCardDoc
	if json.Unmarshal(r.Rendered, &doc) != nil {
		return nil
	}
	labels := map[string]string{controlplane.ResolveKeep: "Keep the files as they are", controlplane.ResolveRestore: "Restore the previous version"}
	var out []approverdevice.Choice
	for _, c := range doc.Choices {
		if l, ok := labels[c]; ok {
			out = append(out, approverdevice.Choice{Value: c, Label: l})
		}
	}
	return out
}

// describeJournalCard is the card's plain view (§18.7).
func describeJournalCard(r persistence.AgentApprovalRequestRow) *approverdevice.Description {
	var doc journalCardDoc
	if err := json.Unmarshal(r.Rendered, &doc); err != nil {
		return &approverdevice.Description{Summary: "A config change was interrupted.", Level: agentadmin.LevelHigh}
	}
	file := controlplane.DriftPath(doc.Detail)
	if file == "" {
		file = "a file"
	}
	what := "The change could not finish and the files are not in a state Vornik can recover on its own."
	switch doc.Drift {
	case controlplane.DriftEditedFile:
		what = file + " was changed outside Vornik while the change was being applied."
	case controlplane.DriftChangedDependency:
		what = file + ", which the change was based on, changed before the change finished."
	}
	reasons := []string{what, "Files in the change: " + strings.Join(doc.Files, ", ") + ".",
		"Keep: Vornik accepts the files on disk now; nothing is written.",
		"Restore: Vornik puts back the version from before the change; a hand edit to those files is overwritten."}
	if doc.Applied {
		reasons = append(reasons, "Restore is not offered: the change is recorded as applied. Keep, then undo it with vornikctl control-plane rollback on the host.")
	}
	reasons = append(reasons, "Projects paused until you decide: "+cardProjects(doc.Projects)+".")
	return &approverdevice.Description{Summary: "A config change for " + cardProjects(doc.Projects) + " was interrupted.",
		Level: agentadmin.LevelHigh, Reasons: reasons}
}

// journalCardFiler builds the filer, or nil when the device service, the
// journal or the proposals are not wired.
func (c *Container) journalCardFiler() *journalCards {
	if c == nil || c.repos == nil || c.repos.ApplyJournal == nil || c.repos.ApproverDevices == nil {
		return nil
	}
	devices := c.approverDeviceService()
	if devices == nil {
		return nil
	}
	return &journalCards{devices: devices, repo: c.repos.ApproverDevices, journal: c.repos.ApplyJournal,
		proposals: c.repos.Proposals, audit: c.repos.AdminAudit,
		resolve: func(ctx context.Context, id, mode, actor string) (*controlplane.ResolveResult, error) {
			if c.journalCardResolveOverride != nil {
				return c.journalCardResolveOverride(ctx, id, mode, actor)
			}
			engine := c.newProposalApplier()
			if engine == nil {
				return nil, errors.New("config journal card: the apply engine is not wired")
			}
			return engine.ResolveJournal(ctx, id, mode, actor)
		},
		now: func() time.Time { return time.Now().UTC() }, logger: c.Logger.With().Str("component", "config-journal-cards").Logger()}
}

// registerJournalCards wires the kind into the device service: the effect,
// the plain view, the choices and the withdrawal opt-in. The filer is built
// at call time, after the device service exists.
func (c *Container) registerJournalCards(svc *approverdevice.Service) {
	kind := persistence.ApprovalKindConfigJournalDrift
	svc.RegisterEffect(kind, func(ctx context.Context, r persistence.AgentApprovalRequestRow) error {
		f := c.journalCardFiler()
		if f == nil {
			return errors.New("config journal card: not wired")
		}
		return f.Effect(ctx, r)
	})
	svc.RegisterDescriber(kind, describeJournalCard)
	svc.RegisterChoices(kind, journalCardChoices)
	svc.RegisterWithdrawable(kind)
}

// journalCardHooks are the engine's OnDrift and OnResolved (§13.3): file the
// card when a row parks, withdraw it when any door resolves the row.
func (c *Container) journalCardHooks(engine *controlplane.ApplyEngine) {
	engine.OnDrift = func(ctx context.Context, journalID string) {
		f := c.journalCardFiler()
		if f == nil {
			return
		}
		// parkInDrift runs under the engine's write locks; filing pushes to
		// the operator's channel, so it runs off that path, bounded.
		go func() {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := f.Ensure(fctx, journalID); err != nil {
				f.logger.Warn().Err(err).Str("journal_id", journalID).Msg("config journal card: filing failed; the sweep retries")
			}
			if c.journalCardAfterFile != nil {
				c.journalCardAfterFile(journalID)
			}
		}()
	}
	engine.OnResolved = func(ctx context.Context, journalID string) {
		if f := c.journalCardFiler(); f != nil {
			f.Withdraw(ctx, journalID, journalCardReasonResolved, "resolved on the host")
		}
	}
}

// StartJournalCardSweep runs the sweep once now (after startup recovery) and
// then every journalCardSweepEvery on the process holding the config-writer
// lease, until ctx ends.
func (c *Container) StartJournalCardSweep(ctx context.Context) {
	f := c.journalCardFiler()
	if f == nil {
		return
	}
	run := func() {
		engine := c.newProposalApplier()
		if engine == nil {
			return
		}
		if ok, _ := leaderelection.DangerousWriteAllowed(ctx, engine.LeaderGate); !ok {
			return
		}
		f.Sweep(ctx)
	}
	run()
	c.journalCardSweepOnce.Do(func() {
		go func() {
			t := time.NewTicker(journalCardSweepEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					run()
				}
			}
		}()
	})
}
