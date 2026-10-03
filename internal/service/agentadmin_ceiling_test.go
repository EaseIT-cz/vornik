package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// Design §18.4: concurrent ceiling approvals stated the wrong total.
// Incident 2026-10-02 (namespace claudecode): three create_project calls made
// against a $102 total each said "approving raises the limit to $104"; the
// user approved all three and list_my_setup reported a $108 namespace sum
// against a $104 ceiling. These tests drive the real service: render, file,
// the device's decision, apply, and the ceiling written by the approval.

// fill creates n inert projects; the fixture's defaults are $2 a project and a
// $10 ceiling, so five fill the namespace to its ceiling.
func (f *agentAdminFixture) fill(slugs ...string) {
	f.t.Helper()
	for _, s := range slugs {
		if res := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: s, Purpose: "Filler " + s}); res.Effect != agentadmin.EffectApplied {
			f.t.Fatalf("create %s: %+v", s, res)
		}
	}
}

// setup is list_my_setup as the agent sees it.
func (f *agentAdminFixture) setup() SetupView {
	f.t.Helper()
	v, err := f.svc.ListSetup(context.Background(), f.key)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// decide approves or rejects a request by id and returns the effect's error.
func (f *agentAdminFixture) decide(id string, approve bool) error {
	f.t.Helper()
	req, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.c.approverDeviceService().Decide(context.Background(), f.device, id, req.RenderedSHA256, approve)
}

// assertCovered is the §7.2 invariant: the namespace sum never exceeds the
// approved ceiling.
func (f *agentAdminFixture) assertCovered(when string) SetupView {
	f.t.Helper()
	v := f.setup()
	if v.BudgetUSD > v.CeilingUSD+1e-9 {
		f.t.Fatalf("%s: namespace sum $%v exceeds the approved ceiling $%v (the 2026-10-02 claudecode $108 vs $104 incident)", when, v.BudgetUSD, v.CeilingUSD)
	}
	return v
}

// approveUntilApplied approves a request; when the apply refuses it because
// its stated maximum was exceeded, it approves the request the daemon filed
// in its place, as the user would.
func (f *agentAdminFixture) approveUntilApplied(id string) {
	f.t.Helper()
	for n := 1; n <= 5; n++ {
		err := f.decide(id, true)
		if err == nil {
			return
		}
		next := f.reRenderedAs(id)
		if next == "" {
			f.t.Fatalf("approval of %s failed and nothing was filed in its place: %v", id, err)
		}
		id = next
	}
	f.t.Fatalf("re-rendering did not converge")
}

// reRenderedAs is the request list_my_setup names as filed in place of a
// failed one, or "".
func (f *agentAdminFixture) reRenderedAs(id string) string {
	f.t.Helper()
	for _, r := range f.setup().Failed {
		if r.ID == id {
			return r.ReRenderedAs
		}
	}
	return ""
}

// Three ceiling-raising requests made against the same total, approved in
// every order: the sum never exceeds the ceiling, and ends equal to it.
// Before the fix, the last approval wrote $12 against a $16 sum (the
// claudecode incident's $104 against $108).
func TestAgentAdmin_CeilingRaisesInEveryOrder(t *testing.T) {
	for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			f := newAgentAdminFixture(t)
			f.fill("aa", "bb", "cc", "dd", "ee") // $10 = the ceiling
			var ids [3]string
			for i, slug := range []string{"xa", "xb", "xc"} {
				res := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: slug, Purpose: "More"})
				if res.Effect != agentadmin.EffectAwaiting {
					t.Fatalf("create %s: %+v", slug, res)
				}
				ids[i] = requestIDOf(res)
			}
			for _, i := range order {
				f.approveUntilApplied(ids[i])
				f.assertCovered(fmt.Sprintf("after approving request %d", i))
			}
			v := f.setup()
			if v.BudgetUSD != 16 || v.CeilingUSD != 16 {
				t.Fatalf("sum $%v ceiling $%v, want both $16", v.BudgetUSD, v.CeilingUSD)
			}
		})
	}
}

// The sentence states the total approving produces and, while other
// spending requests wait, the conditional total.
func TestAgentAdmin_CeilingSentenceStatesTheConditionalTotal(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.fill("aa", "bb", "cc", "dd", "ee")
	first := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"})
	second := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xb", Purpose: "More"})
	if !strings.Contains(first.Sentence, "approving raises the limit to $12.") || strings.Contains(first.Sentence, "up to") {
		t.Fatalf("first sentence %q", first.Sentence)
	}
	if !strings.Contains(second.Sentence, "approving raises the limit to $12 (up to $14 if your other waiting requests that add spending are also approved).") {
		t.Fatalf("second sentence %q", second.Sentence)
	}
}

// A request whose stated maximum is exceeded at apply is refused, applies
// nothing, and is filed again with the true figures; list_my_setup shows both.
func TestAgentAdmin_StatedMaximumExceededIsRefusedAndReRendered(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.fill("aa", "bb", "cc", "dd", "ee")
	a := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"}) // states $12
	b := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xb", Purpose: "More"}) // states $12, up to $14
	if err := f.decide(requestIDOf(b), true); err != nil {
		t.Fatal(err)
	}
	if err := f.decide(requestIDOf(a), true); err == nil {
		t.Fatal("a request whose stated maximum ($12) is exceeded ($14) applied")
	}
	f.assertCovered("after the refused approval")
	if f.c.Registry.GetProject("hermes--xa") != nil {
		t.Fatal("the refused request applied its project")
	}
	if p, _ := f.c.repos.Proposals.GetByID(context.Background(), a.ChangeID); p.Status != persistence.ProposalStatusRejected {
		t.Fatalf("the refused request's proposal is %s", p.Status)
	}
	v := f.setup()
	var failed *SetupRequest
	for i := range v.Failed {
		if v.Failed[i].ID == requestIDOf(a) {
			failed = &v.Failed[i]
		}
	}
	if failed == nil || failed.ReRenderedAs == "" || !strings.Contains(failed.Reason, failed.ReRenderedAs) ||
		!strings.Contains(failed.Reason, "$14") || !strings.Contains(failed.Reason, "$12") {
		t.Fatalf("failed entry %+v", failed)
	}
	var again *SetupRequest
	for i := range v.Pending {
		if v.Pending[i].ID == failed.ReRenderedAs {
			again = &v.Pending[i]
		}
	}
	if again == nil || again.ReRenderOf != requestIDOf(a) || !strings.Contains(again.Sentence, "to $14, above the $12 you approved") {
		t.Fatalf("re-rendered request %+v (pending %+v)", again, v.Pending)
	}
	// The effect is idempotent: a re-run (the re-apply loop) files nothing new.
	req, _ := f.c.repos.ApproverDevices.GetRequest(context.Background(), requestIDOf(a))
	if err := f.svc.applyApproved(context.Background(), *req); err == nil {
		t.Fatal("re-running the refused effect succeeded")
	}
	if n := len(f.setup().Pending); n != 1 {
		t.Fatalf("%d pending requests after re-running the effect, want 1", n)
	}
	if err := f.decide(failed.ReRenderedAs, true); err != nil {
		t.Fatal(err)
	}
	if v := f.assertCovered("after approving the re-rendered request"); v.BudgetUSD != 14 || v.CeilingUSD != 14 {
		t.Fatalf("sum $%v ceiling $%v, want $14", v.BudgetUSD, v.CeilingUSD)
	}
}

// A rejected sibling never forces a re-approval: the survivor applies at once.
func TestAgentAdmin_SiblingRejectedSurvivorAppliesAtOnce(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.fill("aa", "bb", "cc", "dd", "ee")
	a := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"})
	b := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xb", Purpose: "More"})
	if err := f.decide(requestIDOf(a), false); err != nil {
		t.Fatal(err)
	}
	if err := f.decide(requestIDOf(b), true); err != nil {
		t.Fatalf("the survivor of a rejected sibling did not apply: %v", err)
	}
	if v := f.assertCovered("after the survivor"); v.BudgetUSD != 12 || v.CeilingUSD != 12 {
		t.Fatalf("sum $%v ceiling $%v, want $12", v.BudgetUSD, v.CeilingUSD)
	}
}

// The conditional clause counts only requests that add spending: a pending
// device enrolment and a pending credential slot add nothing.
func TestAgentAdmin_ConditionalIgnoresEnrolmentAndCredentialSlot(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	f.fill("aa", "bb", "cc", "dd", "ee")
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "aa", Name: "bank",
		URL: "https://bank.invalid/mcp", Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "FIO"}}))
	if res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "aa", Name: "FIO", Purpose: "read balances", Kind: "secret"}); res.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("request_credential: %+v", res)
	}
	devices := f.c.approverDeviceService()
	code, _, _ := devices.StartPairing(ctx, "Tablet")
	if red, err := devices.Redeem(ctx, code, "10.0.0.2"); err != nil || red.ClaimToken == "" {
		t.Fatalf("a second device did not file an enrolment: %+v, %v", red, err)
	}
	pending, _ := f.c.repos.ApproverDevices.ListPending(ctx, time.Now().UTC())
	kinds := map[string]bool{}
	for _, r := range pending {
		kinds[r.Kind] = true
	}
	if !kinds[persistence.ApprovalKindDeviceEnrollment] || !kinds[persistence.ApprovalKindCredentialSlot] {
		t.Fatalf("the fixture's pending kinds are %v", kinds)
	}
	res := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"})
	if res.Effect != agentadmin.EffectAwaiting || strings.Contains(res.Sentence, "up to") {
		t.Fatalf("an enrolment or a credential slot counted as spending: %q", res.Sentence)
	}
	if next := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xb", Purpose: "More"}); !strings.Contains(next.Sentence, "(up to $14 ") {
		t.Fatalf("the pending project creation was not counted: %q", next.Sentence)
	}
}

// Two raises with DIFFERENT adds approved in each order (review d94f F3): the
// sum never exceeds the ceiling and ends equal to it.
func TestAgentAdmin_DifferentAddsInEachOrder(t *testing.T) {
	for _, bFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("b_first=%v", bFirst), func(t *testing.T) {
			f := newAgentAdminFixture(t)
			f.fill("aa", "bb", "cc", "dd", "ee")
			a := f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "aa", MonthlyUSD: 4}) // +2: $12
			b := f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "bb", MonthlyUSD: 7}) // +5: $15, up to $17
			ids := []string{requestIDOf(a), requestIDOf(b)}
			if bFirst {
				ids[0], ids[1] = ids[1], ids[0]
			}
			for _, id := range ids {
				f.approveUntilApplied(id)
				f.assertCovered("after " + id)
			}
			if v := f.setup(); v.BudgetUSD != 17 || v.CeilingUSD != 17 {
				t.Fatalf("sum $%v ceiling $%v, want $17", v.BudgetUSD, v.CeilingUSD)
			}
		})
	}
}

// Property (§18.4 tests): after any sequence of approvals and rejections of
// spending requests, the namespace sum never exceeds the ceiling, and
// re-rendering converges (F4).
func TestAgentAdmin_CeilingPropertyOverRandomSequences(t *testing.T) {
	const seeds = 8
	reRendered := 0
	for seed := int64(1); seed <= seeds; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			f := newAgentAdminFixture(t)
			f.fill("aa", "bb", "cc", "dd", "ee")
			var open []string
			for i := 0; i < 4; i++ {
				var res agentadmin.Result
				if rng.Intn(2) == 0 {
					res = f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: fmt.Sprintf("n%d", i), Purpose: "More"})
				} else {
					res = f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: []string{"aa", "bb", "cc", "dd"}[i], MonthlyUSD: float64(3 + rng.Intn(6))})
				}
				if res.Effect != agentadmin.EffectAwaiting {
					t.Fatalf("request %d: %+v", i, res)
				}
				open = append(open, requestIDOf(res))
			}
			for steps := 0; len(open) > 0; steps++ {
				if steps > 40 {
					t.Fatal("re-rendering did not converge")
				}
				k := rng.Intn(len(open))
				id := open[k]
				open = append(open[:k], open[k+1:]...)
				if rng.Intn(4) == 0 {
					if err := f.decide(id, false); err != nil {
						t.Fatal(err)
					}
				} else if err := f.decide(id, true); err != nil {
					next := f.reRenderedAs(id)
					if next == "" {
						t.Fatalf("approval failed with nothing filed in its place: %v", err)
					}
					open = append(open, next)
					reRendered++
				}
				f.assertCovered(fmt.Sprintf("step %d", steps))
			}
		})
	}
	// The denominator: sequences that never refused proved nothing about it.
	t.Logf("%d seeds, %d refused and re-rendered", seeds, reRendered)
	if reRendered == 0 {
		t.Fatal("no sequence exercised a refused apply")
	}
}

// A request filed before §18.4 carries only the absolute ceiling_usd it
// pinned (the shape of the claudecode requests of 2026-10-02). It is read as
// the most its sentence stated: alone it applies and raises the ceiling to
// the sum; after a sibling it is refused and re-filed rather than leaving the
// sum above the ceiling. The ceiling is never lowered to the pinned figure.
func TestAgentAdmin_LegacyCeilingPayloadReadAsMaximum(t *testing.T) {
	legacy := func(f *agentAdminFixture, res agentadmin.Result, ceiling float64) persistence.AgentApprovalRequestRow {
		t.Helper()
		row, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), requestIDOf(res))
		if err != nil {
			t.Fatal(err)
		}
		var pl approvalPayload
		if err := json.Unmarshal(row.Rendered, &pl); err != nil {
			t.Fatal(err)
		}
		pl.Grant = agentadmin.Grant{CeilingUSD: &ceiling}
		row.Rendered, _ = json.Marshal(pl)
		row.DecidedByDevice = f.device.ID
		return *row
	}
	t.Run("alone", func(t *testing.T) {
		f := newAgentAdminFixture(t)
		f.fill("aa", "bb", "cc", "dd", "ee")
		a := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"})
		if err := f.svc.applyApproved(context.Background(), legacy(f, a, 12)); err != nil {
			t.Fatal(err)
		}
		if v := f.assertCovered("legacy alone"); v.BudgetUSD != 12 || v.CeilingUSD != 12 {
			t.Fatalf("sum $%v ceiling $%v, want $12", v.BudgetUSD, v.CeilingUSD)
		}
	})
	t.Run("after a sibling", func(t *testing.T) {
		f := newAgentAdminFixture(t)
		f.fill("aa", "bb", "cc", "dd", "ee")
		a := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"})
		b := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xb", Purpose: "More"})
		f.approve(b)
		if err := f.svc.applyApproved(context.Background(), legacy(f, a, 12)); err == nil {
			t.Fatal("a legacy request whose pinned $12 is exceeded applied")
		}
		if f.c.Registry.GetProject("hermes--xa") != nil {
			t.Fatal("the refused legacy request applied")
		}
		if v := f.assertCovered("legacy after a sibling"); v.CeilingUSD != 12 {
			t.Fatalf("ceiling $%v, want $12", v.CeilingUSD)
		}
		// The legacy row was driven through the effect directly, so it is
		// still listed as waiting next to the request filed in its place.
		var again *SetupRequest
		pending := f.setup().Pending
		for i := range pending {
			if pending[i].ReRenderOf == requestIDOf(a) {
				again = &pending[i]
			}
		}
		if again == nil || !strings.Contains(again.Sentence, "to $14, above the $12 you approved") {
			t.Fatalf("the legacy request was not filed again with the true figures: %+v", pending)
		}
	})
}

// failRejectOnce makes the first SetStatus(Rejected) fail, as a crash
// between the two writes of a refusal would.
type failRejectOnce struct {
	persistence.ProposalRepository
	failed bool
}

func (f *failRejectOnce) SetStatus(ctx context.Context, id, status, actor string) error {
	if status == persistence.ProposalStatusRejected && !f.failed {
		f.failed = true
		return errors.New("injected: crash before the reject")
	}
	return f.ProposalRepository.SetStatus(ctx, id, status, actor)
}

// Review e1a4 F2 (2026-10-03): the refusal rejected the proposal and THEN
// filed the re-render, so a crash between the two left a bare rejection the
// retry could not repair (the rejected proposal answers ErrPermanent). The
// re-render is now filed first; the reject follows, and a retry finds what
// was filed.
func TestAgentAdmin_RefusalFilesTheReRenderBeforeRejecting(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.fill("aa", "bb", "cc", "dd", "ee")
	a := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"})
	b := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xb", Purpose: "More"})
	if err := f.decide(requestIDOf(b), true); err != nil {
		t.Fatal(err)
	}
	realProposals := f.svc.proposals
	f.svc.proposals = &failRejectOnce{ProposalRepository: realProposals}
	_ = f.decide(requestIDOf(a), true) // the refusal's reject "crashes"
	reRendered := func() int {
		n := 0
		for _, p := range f.setup().Pending {
			if p.ReRenderOf == requestIDOf(a) {
				n++
			}
		}
		return n
	}
	if n := reRendered(); n != 1 {
		t.Fatalf("after a crash before the reject, %d re-rendered requests are waiting, want 1 (filed first)", n)
	}
	f.svc.proposals = realProposals
	req, _ := f.c.repos.ApproverDevices.GetRequest(context.Background(), requestIDOf(a))
	if err := f.svc.applyApproved(context.Background(), *req); err == nil {
		t.Fatal("the retried refusal succeeded")
	}
	if n := reRendered(); n != 1 {
		t.Fatalf("the retry filed a second re-render: %d waiting", n)
	}
	if p, _ := f.c.repos.Proposals.GetByID(context.Background(), a.ChangeID); p.Status != persistence.ProposalStatusRejected {
		t.Fatalf("after the retry the refused proposal is %s, want rejected", p.Status)
	}
	f.assertCovered("after the crash and retry")
}
