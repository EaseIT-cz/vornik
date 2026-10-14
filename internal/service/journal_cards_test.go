package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/storage"
)

// GitHub #302 (2026-10-13): an interrupted config apply (a DRIFT journal row)
// paused three Hermes projects and only `vornikctl control-plane journal
// resolve` on the host could clear it. Config-apply journal design §13.3:
// one approver-device card per DRIFT row, filed and withdrawn by the daemon
// only, decided with keep or restore through the same engine call.

func journalCardContainer(t *testing.T) *Container {
	t.Helper()
	db := sqlitetest.Memory(t)
	return &Container{
		ConfigPath: t.TempDir() + "/config.yaml",
		repos: &storage.Repositories{
			Proposals:       sqlite.NewProposalRepository(db.DB),
			ApplyJournal:    sqlite.NewApplyJournalRepository(db.DB),
			LeaderLocks:     sqlite.NewLeaderLockRepository(db.DB),
			ApproverDevices: sqlite.NewApproverDeviceRepository(db.DB),
			AdminAudit:      sqlite.NewAdminAuditRepository(db.DB),
		},
	}
}

// parkDriftRow writes a DRIFT row the way parkInDrift leaves one.
func parkDriftRow(t *testing.T, c *Container, id string, projects ...string) {
	t.Helper()
	ctx := context.Background()
	row := &persistence.ConfigApplyJournal{
		ID: id, ProposalID: "prop-" + id, OpDigest: "sha256:ops", DeploymentRoot: "/deploy/configs",
		AffectedProjects: projects,
		Ops:              []persistence.JournalOp{{Op: "replace", Path: "projects/a.yaml", ContentSHA256: "aa"}},
		PreImages:        map[string]persistence.JournalPreImage{"projects/a.yaml": {Existed: true, SHA256: "bb", Content: "x"}},
		ReadSet:          map[string]string{},
	}
	if err := c.repos.ApplyJournal.Prepare(ctx, row); err != nil {
		t.Fatal(err)
	}
	failure := "drift: projects/a.yaml is absent; matches neither pre-image nor intended content"
	for _, st := range [][2]string{{persistence.JournalStatePrepared, persistence.JournalStateReverting}, {persistence.JournalStateReverting, persistence.JournalStateDrift}} {
		if err := c.repos.ApplyJournal.Transition(ctx, id, st[0], st[1], persistence.JournalPatch{Failure: failure}); err != nil {
			t.Fatal(err)
		}
	}
}

func card(t *testing.T, c *Container, journalID string, gen int) *persistence.AgentApprovalRequestRow {
	t.Helper()
	r, err := c.repos.ApproverDevices.GetRequest(context.Background(), journalCardID(journalID, gen))
	if errors.Is(err, persistence.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func auditRows(t *testing.T, c *Container, action string) []*persistence.AdminAuditEntry {
	t.Helper()
	rows, err := c.repos.AdminAudit.List(context.Background(), persistence.AdminAuditFilter{Action: action, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestJournalCards_OnePendingCardPerDriftRow(t *testing.T) {
	c := journalCardContainer(t)
	parkDriftRow(t, c, "caj_1", "hermes--global-news")
	f := c.journalCardFiler()
	if f == nil {
		t.Fatal("no filer")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = f.Ensure(context.Background(), "caj_1") }()
	}
	wg.Wait()
	if err := f.Ensure(context.Background(), "caj_1"); err != nil {
		t.Fatal(err)
	}
	r := card(t, c, "caj_1", 0)
	if r == nil || r.Status != persistence.ApprovalPending || r.Kind != persistence.ApprovalKindConfigJournalDrift || r.Namespace != "" {
		t.Fatalf("card = %+v", r)
	}
	if card(t, c, "caj_1", 1) != nil {
		t.Error("a second card was filed while one is pending")
	}
	if !strings.Contains(r.Sentence, "hermes--global-news") || !r.ExpiresAt.Equal(r.CreatedAt.Add(journalCardTTL)) {
		t.Errorf("sentence %q, expiry %v", r.Sentence, r.ExpiresAt)
	}
	got := journalCardChoices(*r)
	if len(got) != 2 || got[0].Value != "keep" || got[1].Value != "restore" {
		t.Errorf("choices = %+v", got)
	}
	// An agent's namespace never lists it.
	if rows, _ := c.repos.ApproverDevices.ListRecentByNamespace(context.Background(), "hermes", time.Time{}); len(rows) != 0 {
		t.Errorf("an agent namespace lists %d host cards", len(rows))
	}
}

func TestJournalCards_ExpiryReAsksAndAPermanentFailureStops(t *testing.T) {
	c := journalCardContainer(t)
	parkDriftRow(t, c, "caj_2", "alpha")
	f := c.journalCardFiler()
	ctx := context.Background()
	if err := f.Ensure(ctx, "caj_2"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().UTC().Add(journalCardTTL + time.Hour)
	f.now = func() time.Time { return later }
	if err := f.Ensure(ctx, "caj_2"); err != nil {
		t.Fatal(err)
	}
	g1 := card(t, c, "caj_2", 1)
	if g1 == nil || g1.Status != persistence.ApprovalPending {
		t.Fatalf("an expired card must be re-asked as gen 1, got %+v", g1)
	}
	if err := c.repos.ApproverDevices.DecideWithChoice(ctx, g1.ID, g1.RenderedSHA256, "dev", true, "keep", later); err != nil {
		t.Fatal(err)
	}
	if err := c.repos.ApproverDevices.MarkApplyFailed(ctx, g1.ID, "refused", later); err != nil {
		t.Fatal(err)
	}
	if err := f.Ensure(ctx, "caj_2"); err != nil {
		t.Fatal(err)
	}
	if card(t, c, "caj_2", 2) != nil {
		t.Error("a card that failed permanently was re-asked")
	}
}

func TestJournalCards_HostResolveWithdrawsTheCardAndAudits(t *testing.T) {
	c := journalCardContainer(t)
	parkDriftRow(t, c, "caj_3", "alpha")
	ctx := context.Background()
	filed := make(chan string, 1)
	c.journalCardAfterFile = func(id string) { filed <- id }
	engine := c.newProposalApplier()
	engine.OnDrift(ctx, "caj_3") // the park's hook files the card, off the engine's locks
	if id := <-filed; id != "caj_3" {
		t.Fatalf("filed %s", id)
	}
	if r := card(t, c, "caj_3", 0); r == nil || r.Status != persistence.ApprovalPending {
		t.Fatalf("OnDrift did not file the card: %+v", r)
	}
	engine.OnResolved(ctx, "caj_3") // any door's resolve
	r := card(t, c, "caj_3", 0)
	if r.Status != persistence.ApprovalWithdrawn || r.WithdrawnBy != journalCardActor {
		t.Fatalf("card = %+v, want withdrawn by %s", r, journalCardActor)
	}
	if rows := auditRows(t, c, "config_journal.card_withdrawn"); len(rows) != 1 || rows[0].Target != r.ID || rows[0].Principal != "daemon" {
		t.Errorf("withdraw audit = %+v", rows)
	}
	// A repeat changes nothing and audits nothing more.
	engine.OnResolved(ctx, "caj_3")
	if rows := auditRows(t, c, "config_journal.card_withdrawn"); len(rows) != 1 {
		t.Errorf("a repeat withdrawal audited again: %d rows", len(rows))
	}
}

func TestJournalCards_SweepFilesMissingAndWithdrawsResolved(t *testing.T) {
	c := journalCardContainer(t)
	parkDriftRow(t, c, "caj_4", "alpha")
	parkDriftRow(t, c, "caj_5", "beta")
	f := c.journalCardFiler()
	ctx := context.Background()
	f.Sweep(ctx)
	if card(t, c, "caj_4", 0) == nil || card(t, c, "caj_5", 0) == nil {
		t.Fatal("the sweep must file a card for every DRIFT row")
	}
	// caj_5 resolved by a path whose hook never ran (a crash between them).
	if err := c.repos.ApplyJournal.Transition(ctx, "caj_5", persistence.JournalStateDrift, persistence.JournalStateFailed,
		persistence.JournalPatch{Failure: "resolved keep by vadim: x", Terminal: true}); err != nil {
		t.Fatal(err)
	}
	f.Sweep(ctx)
	if r := card(t, c, "caj_5", 0); r.Status != persistence.ApprovalWithdrawn {
		t.Errorf("the resolved row's card = %s, want withdrawn", r.Status)
	}
	if r := card(t, c, "caj_4", 0); r.Status != persistence.ApprovalPending {
		t.Errorf("the DRIFT row's card = %s, want pending", r.Status)
	}
}

// The device decides; the effect runs the engine's resolve with the device
// as actor, audits it with the device as principal, and the resolve's own
// OnResolved (inside the effect) leaves the approved card alone (review
// 1b58 N2: WithdrawTooLate is success).
func TestJournalCards_DeviceDecisionResolvesAuditsAndKeepsItsCard(t *testing.T) {
	c := journalCardContainer(t)
	parkDriftRow(t, c, "caj_6", "alpha")
	ctx := context.Background()
	svc := c.approverDeviceService()
	f := c.journalCardFiler()
	if err := f.Ensure(ctx, "caj_6"); err != nil {
		t.Fatal(err)
	}
	var calls []string
	stub := func(ctx context.Context, id, mode, actor string) (*controlplane.ResolveResult, error) {
		calls = append(calls, id+" "+mode+" "+actor)
		f.Withdraw(ctx, id, journalCardReasonResolved, "resolved on the host") // the engine's OnResolved
		return &controlplane.ResolveResult{JournalID: id, Mode: mode, State: persistence.JournalStateApplied, Changed: true}, nil
	}
	c.journalCardResolveOverride = stub
	code, _, err := svc.StartPairing(ctx, "Pixel")
	if err != nil {
		t.Fatal(err)
	}
	red, err := svc.Redeem(ctx, code, "10.0.0.1")
	if err != nil || red.Device == nil {
		t.Fatalf("pair: %+v %v", red, err)
	}
	r := card(t, c, "caj_6", 0)
	if err := svc.DecideChoice(ctx, red.Device, r.ID, r.RenderedSHA256, "keep"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "caj_6 keep approver device "+red.Device.ID {
		t.Fatalf("resolve calls = %v", calls)
	}
	r = card(t, c, "caj_6", 0)
	if r.Status != persistence.ApprovalApproved || r.AppliedAt == nil || r.ApplyError != "" {
		t.Errorf("card = %+v, want approved and applied", r)
	}
	rows := auditRows(t, c, "config_journal.resolve")
	if len(rows) != 1 || rows[0].Principal != "approver_device:"+red.Device.ID || rows[0].Source != "approver_device" || rows[0].Target != "caj_6" {
		t.Errorf("resolve audit = %+v", rows)
	}
	if n := len(auditRows(t, c, "config_journal.card_withdrawn")); n != 0 {
		t.Errorf("the decided card was audited as withdrawn %d times", n)
	}
	// A choice the card does not offer is refused before anything runs.
	if err := svc.DecideChoice(ctx, red.Device, r.ID, r.RenderedSHA256, "deny"); !errors.Is(err, approverdevice.ErrBadChoice) {
		t.Errorf("deny = %v, want ErrBadChoice", err)
	}
}

// review d040 F5/F6 (the two races of §13.3): the host resolved the row the
// other way before the device's effect ran, and the sweep filed a stray
// card after a host resolve. Neither writes a file: the effect fails the
// card with "already resolved", and the next sweep withdraws the stray.
func TestJournalCards_RacesEndWithOneResolution(t *testing.T) {
	c := journalCardContainer(t)
	parkDriftRow(t, c, "caj_9", "alpha")
	ctx := context.Background()
	svc := c.approverDeviceService()
	f := c.journalCardFiler()
	if err := f.Ensure(ctx, "caj_9"); err != nil {
		t.Fatal(err)
	}
	// The host's keep landed first; the device's restore then reaches a
	// row that is no longer DRIFT.
	c.journalCardResolveOverride = func(context.Context, string, string, string) (*controlplane.ResolveResult, error) {
		return nil, &controlplane.ResolveRefusal{Code: controlplane.RefusalNotDrift, Reason: "not DRIFT", Remedy: "none"}
	}
	code, _, _ := svc.StartPairing(ctx, "Pixel")
	red, err := svc.Redeem(ctx, code, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	r := card(t, c, "caj_9", 0)
	if err := svc.DecideChoice(ctx, red.Device, r.ID, r.RenderedSHA256, "restore"); err == nil || !errors.Is(err, approverdevice.ErrPermanent) {
		t.Fatalf("decide = %v, want the permanent refusal", err)
	}
	r = card(t, c, "caj_9", 0)
	if !strings.Contains(r.ApplyError, "already resolved") {
		t.Errorf("card apply_error = %q", r.ApplyError)
	}

	// The stray: a card filed for a row that the host has since resolved.
	parkDriftRow(t, c, "caj_10", "beta")
	if err := f.Ensure(ctx, "caj_10"); err != nil {
		t.Fatal(err)
	}
	if err := c.repos.ApplyJournal.Transition(ctx, "caj_10", persistence.JournalStateDrift, persistence.JournalStateApplied,
		persistence.JournalPatch{Failure: "resolved keep by vadim: x", Terminal: true}); err != nil {
		t.Fatal(err)
	}
	f.Sweep(ctx)
	if st := card(t, c, "caj_10", 0).Status; st != persistence.ApprovalWithdrawn {
		t.Errorf("stray card = %s, want withdrawn by the sweep", st)
	}
	if card(t, c, "caj_10", 1) != nil {
		t.Error("the sweep filed a card for a resolved row")
	}
}

func TestJournalCards_EffectErrorMapping(t *testing.T) {
	refuse := func(code string) error {
		return &controlplane.ResolveRefusal{Code: code, Reason: "r", Remedy: "m"}
	}
	for _, tc := range []struct {
		err       error
		permanent bool
		contains  string
	}{
		{nil, false, ""},
		{refuse(controlplane.RefusalApplyInProgress), false, ""},
		{refuse(controlplane.RefusalWriterFenced), false, ""},
		{errors.New("db down"), false, ""},
		{refuse(controlplane.RefusalNotDrift), true, "already resolved"},
		{refuse(controlplane.RefusalRestoreApplied), true, "Remedy: m"},
		{refuse(controlplane.RefusalProposalContentChanged), true, "Remedy: m"},
	} {
		got := journalCardEffectError(tc.err)
		if (tc.err == nil) != (got == nil) {
			t.Errorf("%v → %v", tc.err, got)
			continue
		}
		if got == nil {
			continue
		}
		if errors.Is(got, approverdevice.ErrPermanent) != tc.permanent || !strings.Contains(got.Error(), tc.contains) {
			t.Errorf("%v → %v (permanent %v)", tc.err, got, errors.Is(got, approverdevice.ErrPermanent))
		}
	}
}

func TestJournalCards_AppliedProposalOffersKeepOnlyAndTheViewExplains(t *testing.T) {
	c := journalCardContainer(t)
	ctx := context.Background()
	p := &persistence.ControlPlaneProposal{ID: "prop-caj_7", ProjectID: "alpha", Kind: "config", Title: "t", BlastRadius: "project", Status: persistence.ProposalStatusApplied}
	if err := c.repos.Proposals.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.repos.Proposals.GetByID(ctx, "prop-caj_7"); got == nil || got.Status != persistence.ProposalStatusApplied {
		t.Fatalf("setup: proposal = %+v", got)
	}
	parkDriftRow(t, c, "caj_7", "alpha")
	f := c.journalCardFiler()
	if err := f.Ensure(ctx, "caj_7"); err != nil {
		t.Fatal(err)
	}
	r := card(t, c, "caj_7", 0)
	if got := journalCardChoices(*r); len(got) != 1 || got[0].Value != "keep" {
		t.Errorf("choices = %+v, want keep only", got)
	}
	d := describeJournalCard(*r)
	if !strings.Contains(strings.Join(d.Reasons, " "), "Restore is not offered") {
		t.Errorf("view = %+v", d)
	}
}

func TestJournalCards_DescriberNamesTheFileAndTheChoices(t *testing.T) {
	c := journalCardContainer(t)
	parkDriftRow(t, c, "caj_8", "*")
	f := c.journalCardFiler()
	if err := f.Ensure(context.Background(), "caj_8"); err != nil {
		t.Fatal(err)
	}
	r := card(t, c, "caj_8", 0)
	d := describeJournalCard(*r)
	all := d.Summary + " " + strings.Join(d.Reasons, " ")
	for _, want := range []string{"all projects", "projects/a.yaml was changed outside Vornik", "Keep:", "Restore:"} {
		if !strings.Contains(all, want) {
			t.Errorf("view lacks %q: %s", want, all)
		}
	}
	if d.Level != "High" {
		t.Errorf("level = %s", d.Level)
	}
}
