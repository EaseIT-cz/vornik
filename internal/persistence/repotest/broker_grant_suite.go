package repotest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunBrokerGrantSuite pins the standing-grant store on both backends
// (broker write-actions design, "Tier 2: standing grants" as revised, rounds
// 3 and 4, review 61a5). The concurrency cases run on the production pool:
// the SQLite harness is a file database opened with the daemon's DSN, the
// Postgres one the integration lane; never :memory:.
func RunBrokerGrantSuite(t *testing.T, grants persistence.BrokerGrantRepository, actions persistence.BrokerActionRepository) {
	t.Helper()
	h := &standingHarness{grants: grants, actions: actions, ctx: context.Background(), now: time.Now().UTC().Truncate(time.Second)}
	// Final review aa4a, 2026-10-07: the harness used to leave every grant it
	// created live (7-day expiry) in the shared Postgres test DB. They sorted
	// ahead of a later run's grant under DigestDue's ORDER BY digest_through
	// LIMIT 500 and would have failed Gauges_covered_list_and_digest again.
	// Every subtest now revokes what it created (createGrant); this check
	// fails the run if any live grant is left behind (a delta, so a shared DB
	// that already holds other live grants is fine).
	liveBefore := h.liveTotal(t)
	t.Cleanup(func() {
		if after := h.liveTotal(t); after != liveBefore {
			t.Errorf("the suite left %d live grants behind (before %d, after %d)", after-liveBefore, liveBefore, after)
		}
	})
	t.Run("MissContract", func(t *testing.T) { AssertMissRepo(t, "BrokerGrantRepository.Get", grants.Get) })
	t.Run("Seed_approve_creates_the_grant_in_one_transaction", func(t *testing.T) { grantSeedCreates(t, h) })
	t.Run("Stale_seed_hash_creates_nothing", func(t *testing.T) { grantStaleSeed(t, h) })
	t.Run("Live_bound_refuses_and_writes_nothing", func(t *testing.T) { grantLiveBound(t, h) })
	t.Run("Live_bound_holds_under_two_concurrent_seeds", func(t *testing.T) { grantLiveBoundConcurrent(t, h) })
	t.Run("Covered_approve_takes_one_use", func(t *testing.T) { grantCoveredTakesOneUse(t, h) })
	t.Run("Decrement_refuses_a_grant_of_another_key", func(t *testing.T) { grantRefusesAnotherKey(t, h) })
	t.Run("Decrement_refuses_a_grant_of_another_class", func(t *testing.T) { grantRefusesAnotherClass(t, h) })
	t.Run("Pause_revoke_suspend_expiry_and_use_stop_coverage", func(t *testing.T) { grantStatesStopCoverage(t, h) })
	t.Run("Two_actions_on_one_use_exactly_one_covered", func(t *testing.T) { grantTwoActionsOneUse(t, h) })
	t.Run("Revoke_vs_coverage", func(t *testing.T) { grantRevokeVsCoverage(t, h, false) })
	t.Run("Pause_vs_coverage", func(t *testing.T) { grantRevokeVsCoverage(t, h, true) })
	t.Run("A_retry_is_not_a_second_use", func(t *testing.T) { grantRetryIsNotAUse(t, h) })
	t.Run("Only_pre_send_error_refunds_and_only_once", func(t *testing.T) { grantRefund(t, h) })
	t.Run("Gauges_covered_list_and_digest", func(t *testing.T) { grantGaugesAndDigest(t, h) })
	t.Run("Digest_window_boundary_below_a_microsecond", func(t *testing.T) { grantDigestBoundary(t, h) })
	t.Run("Live_grant_beyond_200_dead", func(t *testing.T) { grantLiveBeyondHistory(t, h) })
	t.Run("Digest_due_is_not_starved_by_dead_grants", func(t *testing.T) { grantDigestNotStarved(t, h) })
	t.Run("Dead_grant_owes_its_final_digest", func(t *testing.T) { grantDeadOwesFinalDigest(t, h) })
	t.Run("Dead_tail_is_capped_most_recent_first", func(t *testing.T) { grantDeadTailCapped(t, h) })
}

type standingHarness struct {
	grants  persistence.BrokerGrantRepository
	actions persistence.BrokerActionRepository
	ctx     context.Context
	now     time.Time
}

func (h *standingHarness) liveTotal(t *testing.T) int {
	t.Helper()
	rows, err := h.grants.CountLive(h.ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		n += int(r.Live)
	}
	return n
}

// createGrant is ApproveSeedAndCreate plus a cleanup that revokes the grant
// (through the repository's Revoke) and settles its digest, so neither a live
// nor a digest-owing row outlives the subtest. A failed create is ignored by
// the cleanup: Revoke of a missing id is a no-op or a miss.
func (h *standingHarness) createGrant(t *testing.T, seedActionID, seedHash string, g *persistence.BrokerStandingGrant, limit int, now time.Time) error {
	t.Helper()
	err := h.grants.ApproveSeedAndCreate(h.ctx, seedActionID, seedHash, "device:dev_1", g, limit, now)
	id := g.ID
	t.Cleanup(func() {
		_ = h.grants.Revoke(h.ctx, id, time.Now().UTC())
		if cur, gerr := h.grants.Get(h.ctx, id); gerr == nil {
			_, _, _ = h.grants.AdvanceDigest(h.ctx, id, cur.DigestThrough, time.Now().UTC().Add(48*time.Hour))
		}
	})
	return err
}

// pending stages and promotes one pending action of project/workflow.
func (h *standingHarness) pending(t *testing.T, project, workflow, args string) *persistence.BrokerAction {
	t.Helper()
	a := &persistence.BrokerAction{
		ActionID: uniqueID("bact"), ProjectID: project, TaskID: uniqueID("task"), WorkflowID: workflow,
		ActionKind: "send_reply", Tool: "mcp__mail-write__send", ArgsJSON: []byte(args), ArgsSHA256: "sha-" + uniqueID("a"),
		Status: persistence.BrokerActionStaged, CreatedAt: h.now, ExpiresAt: h.now.Add(24 * time.Hour),
	}
	if _, err := h.actions.Stage(h.ctx, a); err != nil {
		t.Fatal(err)
	}
	if n, err := h.actions.PromoteStaged(h.ctx, a.TaskID); err != nil || n != 1 {
		t.Fatalf("promote: %d %v", n, err)
	}
	return a
}

func (h *standingHarness) grantFor(a *persistence.BrokerAction, keyHash string, uses int) *persistence.BrokerStandingGrant {
	return &persistence.BrokerStandingGrant{
		ID: "bsg_" + a.ActionID, ProjectID: a.ProjectID, Namespace: "", WorkflowID: a.WorkflowID, Action: a.ActionKind,
		KeyPaths: []string{"to"}, KeyValuesSealed: "sv1:sealed", KeyHash: keyHash, MaxUses: uses, UsesLeft: uses,
		ExpiresAt: h.now.Add(7 * 24 * time.Hour), CreatedAt: h.now, CreatedBy: "device:dev_1", SeedActionID: a.ActionID,
		ReachHashAtCreation: "reach-1", Active: true, DigestThrough: h.now,
	}
}

// seeded creates a grant from a fresh seed action and returns it.
func (h *standingHarness) seeded(t *testing.T, project, keyHash string, uses int) *persistence.BrokerStandingGrant {
	t.Helper()
	seed := h.pending(t, project, "wf-mail", `{"to":"a@x.com"}`)
	g := h.grantFor(seed, keyHash, uses)
	if err := h.createGrant(t, seed.ActionID, seed.ArgsSHA256, g, 10, h.now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return g
}

func (h *standingHarness) usesLeft(t *testing.T, id string) int {
	t.Helper()
	g, err := h.grants.Get(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return g.UsesLeft
}

func (h *standingHarness) status(t *testing.T, actionID string) *persistence.BrokerAction {
	t.Helper()
	a, err := h.actions.Get(h.ctx, actionID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func grantSeedCreates(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	g := h.seeded(t, project, "kh-1", 5)
	got, err := h.grants.Get(h.ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsesLeft != 5 || got.MaxUses != 5 || !got.Active || got.Paused || got.SuspendedAt != nil ||
		got.KeyHash != "kh-1" || got.KeyValuesSealed != "sv1:sealed" || len(got.KeyPaths) != 1 || got.KeyPaths[0] != "to" ||
		got.ReachHashAtCreation != "reach-1" || got.CreatedBy != "device:dev_1" || !got.ExpiresAt.Equal(g.ExpiresAt) {
		t.Fatalf("grant read back as %+v", got)
	}
	seed := h.status(t, g.SeedActionID)
	if seed.Status != persistence.BrokerActionApproved || seed.Approver != "device:dev_1" {
		t.Fatalf("seed action %s by %q, want approved by the device", seed.Status, seed.Approver)
	}
}

// Tier 2 revised item 2: a stale seed hash rolls both back.
func grantStaleSeed(t *testing.T, h *standingHarness) {
	seed := h.pending(t, uniqueID("p"), "wf-mail", `{"to":"a@x.com"}`)
	g := h.grantFor(seed, "kh", 5)
	err := h.createGrant(t, seed.ActionID, "sha-stale", g, 10, h.now)
	if !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		t.Fatalf("stale hash: %v, want ErrBrokerActionNoTransition", err)
	}
	if _, err := h.grants.Get(h.ctx, g.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a grant was created from a stale seed: %v", err)
	}
	if a := h.status(t, seed.ActionID); a.Status != persistence.BrokerActionPending {
		t.Fatalf("seed is %s", a.Status)
	}
}

// Round 3 (bounds inside the seed transaction), item 9.
func grantLiveBound(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	h.seeded(t, project, "k1", 5)
	h.seeded(t, project, "k2", 5)
	// Not live: revoked, used up, expired. They do not count.
	rev := h.seeded(t, project, "k3", 5)
	if err := h.grants.Revoke(h.ctx, rev.ID, h.now); err != nil {
		t.Fatal(err)
	}
	seedOld := h.pending(t, project, "wf-mail", `{}`)
	old := h.grantFor(seedOld, "k4", 5)
	old.ExpiresAt = h.now.Add(-time.Minute)
	if err := h.createGrant(t, seedOld.ActionID, seedOld.ArgsSHA256, old, 3, h.now); err != nil {
		t.Fatalf("an expired grant is not live and must not hit the bound: %v", err)
	}
	seed := h.pending(t, project, "wf-mail", `{}`)
	g := h.grantFor(seed, "k5", 5)
	if err := h.createGrant(t, seed.ActionID, seed.ArgsSHA256, g, 2, h.now); !errors.Is(err, persistence.ErrBrokerGrantLimit) {
		t.Fatalf("third live grant with a bound of 2: %v, want ErrBrokerGrantLimit", err)
	}
	if _, err := h.grants.Get(h.ctx, g.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("a refused grant was written")
	}
	if a := h.status(t, seed.ActionID); a.Status != persistence.BrokerActionPending {
		t.Fatalf("the seed of a refused grant is %s; nothing may be written", a.Status)
	}
	// Another project has its own bound.
	h.seeded(t, uniqueID("p"), "k1", 5)
}

// grantHook is the deterministic race seam both drivers expose.
type grantHook interface {
	SetGrantHookForTest(func(stage string))
}

// holdAll makes each of n concurrent transactions wait at the hook until
// all n arrived (or 300 ms passed, when the driver already serialised them),
// so an unserialised count or decrement is caught every run.
func holdAll(t *testing.T, repo persistence.BrokerGrantRepository, n int, stage string) func() {
	t.Helper()
	hooked, ok := repo.(grantHook)
	if !ok {
		t.Fatalf("%T has no grant hook; the race cannot be made deterministic", repo)
	}
	var arrived sync.WaitGroup
	arrived.Add(n)
	hooked.SetGrantHookForTest(func(s string) {
		if s != stage {
			return
		}
		arrived.Done()
		done := make(chan struct{})
		go func() { arrived.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
		}
	})
	return func() { hooked.SetGrantHookForTest(nil) }
}

// Round 4 F3 / review 61a5 F4: two seed approvals at the limit, exactly one
// grant created.
func grantLiveBoundConcurrent(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	h.seeded(t, project, "k0", 5)
	seeds := []*persistence.BrokerAction{h.pending(t, project, "wf-mail", `{}`), h.pending(t, project, "wf-mail", `{}`)}
	defer holdAll(t, h.grants, 2, "seed-counted")()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, s := range seeds {
		wg.Add(1)
		go func(i int, s *persistence.BrokerAction) {
			defer wg.Done()
			errs[i] = h.createGrant(t, s.ActionID, s.ArgsSHA256, h.grantFor(s, "k", 5), 2, h.now)
		}(i, s)
	}
	wg.Wait()
	created := 0
	for i, err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, persistence.ErrBrokerGrantLimit):
			if a := h.status(t, seeds[i].ActionID); a.Status != persistence.BrokerActionPending {
				t.Fatalf("the refused seed is %s", a.Status)
			}
		default:
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if created != 1 {
		t.Fatalf("%d grants created at a bound of 2 with one live; want exactly 1", created)
	}
}

func grantCoveredTakesOneUse(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	g := h.seeded(t, project, "kh", 3)
	a := h.pending(t, project, "wf-mail", `{"to":"a@x.com","body":"hi"}`)
	if err := h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, "kh", h.now); err != nil {
		t.Fatalf("covered approve: %v", err)
	}
	got := h.status(t, a.ActionID)
	if got.Status != persistence.BrokerActionApproved || got.Approver != persistence.BrokerGrantApprover(g.ID) {
		t.Fatalf("covered action is %s by %q", got.Status, got.Approver)
	}
	if n := h.usesLeft(t, g.ID); n != 2 {
		t.Fatalf("uses_left = %d, want 2", n)
	}
	// A stale args hash approves nothing and takes nothing.
	b := h.pending(t, project, "wf-mail", `{}`)
	if err := h.grants.ApproveUnderGrant(h.ctx, b.ActionID, "sha-other", g.ID, "kh", h.now); !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		t.Fatalf("stale args hash: %v", err)
	}
	if n := h.usesLeft(t, g.ID); n != 2 {
		t.Fatalf("a refused approve took a use: %d", n)
	}
}

// Round 4 F1: the statement refuses unless the action's key is the one the
// person approved, whatever grant id the matcher chose.
func grantRefusesAnotherKey(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	g := h.seeded(t, project, "kh-jana", 3)
	a := h.pending(t, project, "wf-mail", `{"to":"eve@x.com"}`)
	err := h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, "kh-eve", h.now)
	if !errors.Is(err, persistence.ErrBrokerGrantNotCovered) {
		t.Fatalf("another key: %v, want ErrBrokerGrantNotCovered", err)
	}
	if got := h.status(t, a.ActionID); got.Status != persistence.BrokerActionPending || got.Approver != "" {
		t.Fatalf("the action was approved under another key's grant: %s %q", got.Status, got.Approver)
	}
	if n := h.usesLeft(t, g.ID); n != 3 {
		t.Fatalf("uses_left = %d", n)
	}
}

// The decrement also binds the grant's class: the action's own project,
// workflow and action.
func grantRefusesAnotherClass(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	g := h.seeded(t, project, "kh", 3)
	for _, a := range []*persistence.BrokerAction{
		h.pending(t, project, "wf-other", `{}`),
		h.pending(t, uniqueID("p"), "wf-mail", `{}`),
	} {
		if err := h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, "kh", h.now); !errors.Is(err, persistence.ErrBrokerGrantNotCovered) {
			t.Fatalf("%s/%s: %v, want ErrBrokerGrantNotCovered", a.ProjectID, a.WorkflowID, err)
		}
		if got := h.status(t, a.ActionID); got.Status != persistence.BrokerActionPending {
			t.Fatalf("approved across classes: %s", got.Status)
		}
	}
}

// Rounds 3 and 4: pause, revoke, suspension, expiry with no sweep, and the
// last use each stop coverage; unpause and confirm restore it; revoke is
// final.
func grantStatesStopCoverage(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	cover := func(g *persistence.BrokerStandingGrant, at time.Time) error {
		a := h.pending(t, project, "wf-mail", `{}`)
		err := h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, g.KeyHash, at)
		if err != nil && h.status(t, a.ActionID).Status != persistence.BrokerActionPending {
			t.Fatal("a refused cover left the action approved")
		}
		return err
	}
	g := h.seeded(t, project, "kh", 5)
	if err := h.grants.SetPaused(h.ctx, g.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := cover(g, h.now); !errors.Is(err, persistence.ErrBrokerGrantNotCovered) {
		t.Fatalf("paused: %v", err)
	}
	if err := h.grants.SetPaused(h.ctx, g.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := cover(g, h.now); err != nil {
		t.Fatalf("unpaused: %v", err)
	}
	if ok, err := h.grants.Suspend(h.ctx, g.ID, h.now); err != nil || !ok {
		t.Fatalf("suspend: %v %v", ok, err)
	}
	if ok, _ := h.grants.Suspend(h.ctx, g.ID, h.now); ok {
		t.Fatal("a second suspend reported a transition")
	}
	if err := cover(g, h.now); !errors.Is(err, persistence.ErrBrokerGrantNotCovered) {
		t.Fatalf("suspended: %v", err)
	}
	if err := h.grants.Confirm(h.ctx, g.ID, "reach-2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.grants.Get(h.ctx, g.ID); got.SuspendedAt != nil || got.ReachHashAtCreation != "reach-2" {
		t.Fatalf("confirm did not clear and re-pin: %+v", got)
	}
	if err := cover(g, h.now); err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	// Expiry needs no sweep: the statement compares expires_at with now.
	seedShort := h.pending(t, project, "wf-mail", `{}`)
	short := h.grantFor(seedShort, "ks", 5)
	short.ExpiresAt = h.now.Add(time.Hour)
	if err := h.createGrant(t, seedShort.ActionID, seedShort.ArgsSHA256, short, 10, h.now); err != nil {
		t.Fatal(err)
	}
	if err := cover(short, short.ExpiresAt.Add(-time.Second)); err != nil {
		t.Fatalf("a second before expiry: %v", err)
	}
	if err := cover(short, short.ExpiresAt); !errors.Is(err, persistence.ErrBrokerGrantNotCovered) {
		t.Fatalf("at expiry: %v", err)
	}
	if err := h.grants.Revoke(h.ctx, g.ID, h.now); err != nil {
		t.Fatal(err)
	}
	if err := cover(g, h.now); !errors.Is(err, persistence.ErrBrokerGrantNotCovered) {
		t.Fatalf("revoked: %v", err)
	}
	if err := h.grants.SetPaused(h.ctx, g.ID, false); !errors.Is(err, persistence.ErrBrokerGrantNoTransition) {
		t.Fatalf("unpausing a revoked grant: %v", err)
	}
	if err := h.grants.Confirm(h.ctx, g.ID, "x"); !errors.Is(err, persistence.ErrBrokerGrantNoTransition) {
		t.Fatalf("confirming a revoked grant: %v", err)
	}
	if err := h.grants.Revoke(h.ctx, g.ID, h.now); !errors.Is(err, persistence.ErrBrokerGrantNoTransition) {
		t.Fatalf("a second revoke: %v", err)
	}
	// The last use: one left, two actions, the second is refused.
	one := h.seeded(t, project, "kh1", 1)
	if err := cover(one, h.now); err != nil {
		t.Fatal(err)
	}
	if err := cover(one, h.now); !errors.Is(err, persistence.ErrBrokerGrantNotCovered) {
		t.Fatalf("used up: %v", err)
	}
}

// Round 4 F6: two concurrent actions on one remaining use. One is approved
// grant:<id>, the other stays pending (the caller then files its ordinary
// request).
func grantTwoActionsOneUse(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	g := h.seeded(t, project, "kh", 1)
	acts := []*persistence.BrokerAction{h.pending(t, project, "wf-mail", `{}`), h.pending(t, project, "wf-mail", `{}`)}
	defer holdAll(t, h.grants, 2, "action-approved")()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, a := range acts {
		wg.Add(1)
		go func(i int, a *persistence.BrokerAction) {
			defer wg.Done()
			errs[i] = h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, "kh", h.now)
		}(i, a)
	}
	wg.Wait()
	covered, pending := 0, 0
	for i, a := range acts {
		got := h.status(t, a.ActionID)
		switch {
		case errs[i] == nil && got.Status == persistence.BrokerActionApproved && got.Approver == persistence.BrokerGrantApprover(g.ID):
			covered++
		case errors.Is(errs[i], persistence.ErrBrokerGrantNotCovered) && got.Status == persistence.BrokerActionPending:
			pending++
		default:
			t.Fatalf("action %d: err %v, status %s", i, errs[i], got.Status)
		}
	}
	if covered != 1 || pending != 1 {
		t.Fatalf("covered %d, pending %d; want exactly one of each", covered, pending)
	}
	if n := h.usesLeft(t, g.ID); n != 0 {
		t.Fatalf("uses_left = %d", n)
	}
}

// Rounds 3 and 4: revoke (or pause) racing a covered approval. Whatever the
// interleaving, a covered action has taken exactly one use, and after the
// revoke nothing more is covered.
func grantRevokeVsCoverage(t *testing.T, h *standingHarness, pause bool) {
	project := uniqueID("p")
	g := h.seeded(t, project, "kh", 5)
	a := h.pending(t, project, "wf-mail", `{}`)
	restore := holdAll(t, h.grants, 2, "action-approved")
	var wg sync.WaitGroup
	var coverErr, changeErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		coverErr = h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, "kh", h.now)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		if pause {
			changeErr = h.grants.SetPaused(h.ctx, g.ID, true)
		} else {
			changeErr = h.grants.Revoke(h.ctx, g.ID, h.now)
		}
	}()
	wg.Wait()
	restore()
	if changeErr != nil {
		t.Fatalf("change: %v", changeErr)
	}
	got := h.status(t, a.ActionID)
	left := h.usesLeft(t, g.ID)
	switch {
	case coverErr == nil:
		if got.Status != persistence.BrokerActionApproved || left != 4 {
			t.Fatalf("covered, but status %s and uses_left %d", got.Status, left)
		}
	case errors.Is(coverErr, persistence.ErrBrokerGrantNotCovered):
		if got.Status != persistence.BrokerActionPending || left != 5 {
			t.Fatalf("not covered, but status %s and uses_left %d", got.Status, left)
		}
	default:
		t.Fatalf("cover: %v", coverErr)
	}
	next := h.pending(t, project, "wf-mail", `{}`)
	if err := h.grants.ApproveUnderGrant(h.ctx, next.ActionID, next.ArgsSHA256, g.ID, "kh", h.now); !errors.Is(err, persistence.ErrBrokerGrantNotCovered) {
		t.Fatalf("after the change the next action was covered: %v", err)
	}
}

// Round 4 F2: the decrement runs only when the action's own transition
// affected one row. A covered approve takes N to N-1; re-running it (a
// worker retry, a re-applied effect) leaves N-1; the worker's claim and
// finish take nothing.
func grantRetryIsNotAUse(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	g := h.seeded(t, project, "kh", 5)
	a := h.pending(t, project, "wf-mail", `{}`)
	if err := h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, "kh", h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, "kh", h.now); !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		t.Fatalf("re-approve: %v", err)
	}
	if ok, err := h.actions.ClaimForExecution(h.ctx, a.ActionID, h.now); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, _ := h.actions.ClaimForExecution(h.ctx, a.ActionID, h.now); ok {
		t.Fatal("a second claim won")
	}
	if n := h.usesLeft(t, g.ID); n != 4 {
		t.Fatalf("uses_left = %d after one covered approve and a retry; want 4", n)
	}
}

// Tier 2 revised item 5, round 3 tests, round 4 F2, review 61a5 F2: only
// pre_send_error refunds; a retried pre_send_error leaves N, not N+1; across
// a storm uses_left never exceeds max_uses.
func grantRefund(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	g := h.seeded(t, project, "kh", 2)
	run := func(status, class string) *persistence.BrokerAction {
		a := h.pending(t, project, "wf-mail", `{}`)
		if err := h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, "kh", h.now); err != nil {
			t.Fatalf("cover: %v", err)
		}
		if ok, err := h.actions.ClaimForExecution(h.ctx, a.ActionID, h.now); !ok || err != nil {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if err := h.actions.Finish(h.ctx, a.ActionID, status, class, []byte(`{}`), h.now); err != nil {
			t.Fatalf("finish: %v", err)
		}
		return a
	}
	run(persistence.BrokerActionUnknown, persistence.BrokerOutcomeTimeout)
	if n := h.usesLeft(t, g.ID); n != 1 {
		t.Fatalf("a timeout refunded: uses_left %d", n)
	}
	a := run(persistence.BrokerActionFailed, persistence.BrokerOutcomePreSendError)
	if n := h.usesLeft(t, g.ID); n != 1 {
		t.Fatalf("pre_send_error: uses_left %d, want the use back (1)", n)
	}
	// The retried terminal write does not transition, so it does not refund.
	for i := 0; i < 5; i++ {
		_ = h.actions.Finish(h.ctx, a.ActionID, persistence.BrokerActionFailed, persistence.BrokerOutcomePreSendError, []byte(`{}`), h.now)
	}
	if n := h.usesLeft(t, g.ID); n != 1 {
		t.Fatalf("a retried pre_send_error refunded again: %d", n)
	}
	run(persistence.BrokerActionFailed, persistence.BrokerOutcomeToolError)
	if n := h.usesLeft(t, g.ID); n != 0 {
		t.Fatalf("a tool error refunded: %d", n)
	}
	// An action not approved under a grant refunds nothing.
	plain := h.pending(t, project, "wf-mail", `{}`)
	if err := h.actions.Approve(h.ctx, plain.ActionID, plain.ArgsSHA256, "operator", h.now); err != nil {
		t.Fatal(err)
	}
	_, _ = h.actions.ClaimForExecution(h.ctx, plain.ActionID, h.now)
	_ = h.actions.Finish(h.ctx, plain.ActionID, persistence.BrokerActionFailed, persistence.BrokerOutcomePreSendError, nil, h.now)
	if n := h.usesLeft(t, g.ID); n != 0 {
		t.Fatalf("an uncovered action refunded the grant: %d", n)
	}
	// Never above max_uses: a full grant refunds to its maximum only.
	full := h.seeded(t, project, "kf", 1)
	b := h.pending(t, project, "wf-mail", `{}`)
	_ = h.grants.ApproveUnderGrant(h.ctx, b.ActionID, b.ArgsSHA256, full.ID, "kf", h.now)
	_, _ = h.actions.ClaimForExecution(h.ctx, b.ActionID, h.now)
	_ = h.actions.Finish(h.ctx, b.ActionID, persistence.BrokerActionFailed, persistence.BrokerOutcomePreSendError, nil, h.now)
	if n := h.usesLeft(t, full.ID); n != 1 {
		t.Fatalf("uses_left %d, want 1 (max_uses)", n)
	}
}

func grantGaugesAndDigest(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	g := h.seeded(t, project, "kh", 5)
	p := h.seeded(t, project, "kp", 5)
	if err := h.grants.SetPaused(h.ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	rows, err := h.grants.CountLive(h.ctx, h.now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.ProjectID == project {
			found = true
			if r.Live != 2 || r.Paused != 1 {
				t.Fatalf("gauge row %+v, want 2 live, 1 paused", r)
			}
		}
	}
	if !found {
		t.Fatal("no gauge row for the project")
	}
	// Two covered actions: listed, and counted by the digest exactly once.
	for i := 0; i < 2; i++ {
		a := h.pending(t, project, "wf-mail", `{}`)
		if err := h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, "kh", h.now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	covered, err := h.grants.CoveredActions(h.ctx, g.ID, 10)
	if err != nil || len(covered) != 2 {
		t.Fatalf("CoveredActions = %d, %v", len(covered), err)
	}
	due, err := h.grants.DigestDue(h.ctx, h.now, h.now)
	if err != nil {
		t.Fatal(err)
	}
	var dg *persistence.BrokerStandingGrant
	for _, d := range due {
		if d.ID == g.ID {
			dg = d
		}
	}
	if dg == nil {
		t.Fatal("the grant is not due for its digest")
	}
	to := h.now.Add(time.Hour)
	n, ok, err := h.grants.AdvanceDigest(h.ctx, g.ID, dg.DigestThrough, to)
	if err != nil || !ok || n != 2 {
		t.Fatalf("AdvanceDigest = %d %v %v, want 2", n, ok, err)
	}
	if n, ok, _ := h.grants.AdvanceDigest(h.ctx, g.ID, dg.DigestThrough, to); ok || n != 0 {
		t.Fatalf("a second pass over the same window counted %d (ok %v)", n, ok)
	}
	// Lists for pages: operator projects by id; every agent namespace ("*").
	ops, err := h.grants.List(h.ctx, persistence.BrokerGrantFilter{ProjectIDs: []string{project}})
	if err != nil || len(ops) != 2 {
		t.Fatalf("List = %d %v", len(ops), err)
	}
	grantAllAgentNamespaces(t, h)
	cands, err := h.grants.ListForAction(h.ctx, project, "wf-mail", "send_reply", h.now)
	if err != nil || len(cands) != 2 {
		t.Fatalf("ListForAction = %d %v", len(cands), err)
	}
}

// grantAllAgentNamespaces: the device page's filter lists agent grants only.
func grantAllAgentNamespaces(t *testing.T, h *standingHarness) {
	t.Helper()
	agentProject := uniqueID("ns") + "--mail"
	seedA := h.pending(t, agentProject, "wf-mail", `{}`)
	ga := h.grantFor(seedA, "ka", 5)
	ga.Namespace = "nsa"
	if err := h.createGrant(t, seedA.ActionID, seedA.ArgsSHA256, ga, 10, h.now); err != nil {
		t.Fatal(err)
	}
	all, err := h.grants.List(h.ctx, persistence.BrokerGrantFilter{Namespace: persistence.BrokerGrantAllAgentNamespaces, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	foundAgent := false
	for _, g := range all {
		if g.Namespace == "" {
			t.Fatalf("the all-agent-namespaces list holds an operator grant %s", g.ID)
		}
		foundAgent = foundAgent || g.ID == ga.ID
	}
	if !foundAgent {
		t.Fatal("the all-agent-namespaces list misses an agent grant")
	}
}

// Review f819 item 4: a covered action approved at an instant with a
// sub-microsecond part is counted by the digest window that ends exactly
// then. Postgres keeps microseconds (and rounds), so ApproveUnderGrant
// stamps decided_at truncated to the microsecond, as AdvanceDigest
// truncates its bounds: the stored value and the strict window agree.
func grantDigestBoundary(t *testing.T, h *standingHarness) {
	project := uniqueID("p")
	g := h.seeded(t, project, "kb", 5)
	at := h.now.Add(time.Second + 999*time.Nanosecond)
	a := h.pending(t, project, "wf-mail", `{}`)
	if err := h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, "kb", at); err != nil {
		t.Fatal(err)
	}
	n, ok, err := h.grants.AdvanceDigest(h.ctx, g.ID, g.DigestThrough, at)
	if err != nil || !ok || n != 1 {
		t.Fatalf("the window ending at the approval counted %d (ok %v, %v), want 1", n, ok, err)
	}
	// The next window, starting where the first ended, does not count it
	// again.
	got, err := h.grants.Get(h.ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	n, ok, err = h.grants.AdvanceDigest(h.ctx, g.ID, got.DigestThrough, at.Add(time.Hour))
	if err != nil || !ok || n != 0 {
		t.Fatalf("the next window counted %d (ok %v, %v), want 0", n, ok, err)
	}
}

// insertDead creates n expired grants of one class, the i-th expiring
// (base - i minutes), through the repository's own create path
// (ApproveSeedAndCreate with a past expiry): a dead grant is not counted
// by the live ceiling, so no bypass is needed. Returns the grant ids in
// creation order (most recent expiry first).
func (h *standingHarness) insertDead(t *testing.T, project string, n int, base, digestThrough time.Time) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		seed := h.pending(t, project, "wf-mail", `{"to":"a@x.com"}`)
		g := h.grantFor(seed, fmt.Sprintf("k%03d", i), 1)
		g.ExpiresAt = base.Add(-time.Duration(i) * time.Minute)
		g.DigestThrough = digestThrough.Add(-time.Duration(i) * time.Minute)
		if err := h.createGrant(t, seed.ActionID, seed.ArgsSHA256, g, 10, h.now); err != nil {
			t.Fatalf("dead grant %d: %v", i, err)
		}
		ids = append(ids, g.ID)
	}
	return ids
}

// GitHub #78 (2026-10-05 audit): ListForAction sorted by expiry and cut at 200
// before the matcher filtered, so 200 expired grants hid a new live one.
// The dead grants are created through ApproveSeedAndCreate with a past expiry.
func grantLiveBeyondHistory(t *testing.T, h *standingHarness) {
	project := uniqueID("p78")
	h.insertDead(t, project, 200, h.now.Add(-time.Hour), h.now)
	live := h.seeded(t, project, "klive", 1)
	cands, err := h.grants.ListForAction(h.ctx, project, "wf-mail", "send_reply", h.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) == 0 || cands[0].ID != live.ID {
		t.Fatalf("the live grant is not first in %d candidates", len(cands))
	}
	if len(cands) > 201 {
		t.Fatalf("%d candidates, want the live grant plus at most 200 dead", len(cands))
	}
}

// GitHub #78: the dead tail is the 200 MOST RECENT dead grants, in that
// order, after every live grant (sooner expiry first). The oldest dead
// grant of 201 is the one that falls off.
func grantDeadTailCapped(t *testing.T, h *standingHarness) {
	project := uniqueID("p78t")
	dead := h.insertDead(t, project, 201, h.now.Add(-time.Hour), h.now)
	liveLate := h.seeded(t, project, "klate", 1)
	liveSoon := h.grantFor(h.pending(t, project, "wf-mail", `{}`), "ksoon", 1)
	liveSoon.ExpiresAt = h.now.Add(time.Hour)
	if err := h.createGrant(t, liveSoon.SeedActionID, h.status(t, liveSoon.SeedActionID).ArgsSHA256, liveSoon, 10, h.now); err != nil {
		t.Fatal(err)
	}
	cands, err := h.grants.ListForAction(h.ctx, project, "wf-mail", "send_reply", h.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 202 {
		t.Fatalf("%d candidates, want 2 live + 200 dead", len(cands))
	}
	if cands[0].ID != liveSoon.ID || cands[1].ID != liveLate.ID {
		t.Fatalf("live order %s, %s; want the sooner-expiring first", cands[0].ID, cands[1].ID)
	}
	for i, g := range cands[2:] {
		if g.ID != dead[i] {
			t.Fatalf("dead tail position %d is %s, want %s (most recent expiry first)", i, g.ID, dead[i])
		}
	}
}

// GitHub #78 sibling (fix round 1, found in review): DigestDue ordered by
// digest_through with a global LIMIT 500 and no liveness filter, so 500
// dead grants with an old digest_through starved every live grant's digest.
// A dead grant owes a digest only while covered actions are uncounted.
// The dead grants are inert rows left in the shared database (the
// repository has no delete); the filter is what keeps them from affecting
// any other test's DigestDue.
func grantDigestNotStarved(t *testing.T, h *standingHarness) {
	project := uniqueID("p78d")
	h.insertDead(t, project, 501, h.now.Add(-100*time.Hour), h.now.Add(-72*time.Hour))
	live := h.seeded(t, project, "kd", 5)
	due, err := h.grants.DigestDue(h.ctx, h.now, h.now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range due {
		if d.ID == live.ID {
			found = true
		}
		if d.Namespace == "" && d.ProjectID == project && d.ID != live.ID {
			t.Fatalf("dead grant %s with nothing uncounted is still due", d.ID)
		}
	}
	if !found {
		t.Fatalf("the live grant is not due behind 501 dead ones (%d returned)", len(due))
	}
}

// A dead grant that still has uncounted covered actions owes its final
// digest, and stops being due once it is advanced.
func grantDeadOwesFinalDigest(t *testing.T, h *standingHarness) {
	project := uniqueID("p78f")
	g := h.seeded(t, project, "kf", 5)
	a := h.pending(t, project, "wf-mail", `{}`)
	if err := h.grants.ApproveUnderGrant(h.ctx, a.ActionID, a.ArgsSHA256, g.ID, "kf", h.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := h.grants.Revoke(h.ctx, g.ID, h.now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	isDue := func() *persistence.BrokerStandingGrant {
		due, err := h.grants.DigestDue(h.ctx, h.now.Add(time.Hour), h.now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range due {
			if d.ID == g.ID {
				return d
			}
		}
		return nil
	}
	d := isDue()
	if d == nil {
		t.Fatal("a revoked grant with an uncounted covered action owes its final digest")
	}
	if n, ok, err := h.grants.AdvanceDigest(h.ctx, g.ID, d.DigestThrough, h.now.Add(time.Hour)); err != nil || !ok || n != 1 {
		t.Fatalf("AdvanceDigest = %d %v %v", n, ok, err)
	}
	if isDue() != nil {
		t.Fatal("a revoked grant with nothing uncounted is still due")
	}
}
