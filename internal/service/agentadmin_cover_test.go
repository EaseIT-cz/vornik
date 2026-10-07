package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// Design §18.4 F10: a namespace already above its approved ceiling.
// Incident 2026-10-02 (namespace claudecode): three approvals left the
// projects summing to $108 against a $104 ceiling, and nothing reported or
// covered the gap. The daemon files a cover request ("approving sets the
// limit to the sum"), blocks every change that would grow the sum meanwhile,
// and the doctor names the namespace. These tests force the state the
// incident left by lowering the ceiling under the sum.

// setCeiling writes the namespace's approved ceiling as an operator would.
func (f *agentAdminFixture) setCeiling(v float64) {
	f.t.Helper()
	if err := f.c.repos.AgentGrants.UpsertCeiling(context.Background(), persistence.AgentNamespaceBudget{
		Namespace: f.key.AgentNamespace, CeilingUSD: v, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		f.t.Fatal(err)
	}
}

// forceOver fills the namespace to $10 and lowers the ceiling to ceiling.
func (f *agentAdminFixture) forceOver(ceiling float64) {
	f.t.Helper()
	f.fill("aa", "bb", "cc", "dd", "ee")
	f.setCeiling(ceiling)
}

// read is an agent's read verb (list_my_setup through the API).
func (f *agentAdminFixture) read() {
	f.t.Helper()
	if _, err := f.svc.ListSetupJSON(context.Background(), f.key); err != nil {
		f.t.Fatal(err)
	}
}

// coverRow is a cover request with its decoded payload.
type coverRow struct {
	persistence.AgentApprovalRequestRow
	pl approvalPayload
}

// covers lists the namespace's cover requests, newest first.
func (f *agentAdminFixture) covers() []coverRow {
	f.t.Helper()
	rows, err := f.c.repos.ApproverDevices.ListRecentByNamespace(context.Background(), f.key.AgentNamespace, time.Time{})
	if err != nil {
		f.t.Fatal(err)
	}
	var out []coverRow
	for _, r := range rows {
		var pl approvalPayload
		if json.Unmarshal(r.Rendered, &pl) == nil && pl.Grant.Cover {
			out = append(out, coverRow{r, pl})
		}
	}
	return out
}

// pendingCovers is the cover requests still waiting for a decision.
func (f *agentAdminFixture) pendingCovers() []coverRow {
	f.t.Helper()
	var out []coverRow
	for _, c := range f.covers() {
		if c.Status == persistence.ApprovalPending {
			out = append(out, c)
		}
	}
	return out
}

// onePendingCover asserts exactly one waiting cover request and returns it.
func (f *agentAdminFixture) onePendingCover(when string) coverRow {
	f.t.Helper()
	p := f.pendingCovers()
	if len(p) != 1 {
		f.t.Fatalf("%s: %d cover requests waiting, want 1 (all covers: %d)", when, len(p), len(f.covers()))
	}
	return p[0]
}

func coverSentence(total, ceiling string) string {
	return "Your assistant's projects add up to $" + total + " a month, above the $" + ceiling + " limit now in force. " +
		"Approving sets the limit to $" + total + ". To keep $" + ceiling + " instead, reject this and ask your assistant to lower a budget."
}

// budget is the doctor source's row for the fixture's namespace.
func (f *agentAdminFixture) budget() api.AgentNamespaceBudgetStatus {
	ns := f.key.AgentNamespace
	f.t.Helper()
	rows, err := f.c.agentNamespaceBudgets(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	for _, r := range rows {
		if r.Namespace == ns {
			return r
		}
	}
	f.t.Fatalf("the doctor's source did not examine namespace %s: %+v", ns, rows)
	return api.AgentNamespaceBudgetStatus{}
}

// The next verb, read or write, files exactly one cover request; another verb
// files no second one. Its sentence names no cause (two exist: the
// pre-§18.4 race and an operator lowering the ceiling) and its grant pins the
// sum as filed, adds nothing, and carries no file operation.
func TestAgentAdmin_CoverFiledOnceOnTheNextVerb(t *testing.T) {
	egress := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"total":{"type":"number"}}}`)
	slugs := 0
	verbs := map[string]func(f *agentAdminFixture){
		"list_my_setup": func(f *agentAdminFixture) { f.read() },
		"describe_installation": func(f *agentAdminFixture) {
			if _, err := f.svc.DescribeJSON(context.Background(), f.key); err != nil {
				f.t.Fatal(err)
			}
		},
		"catalog": func(f *agentAdminFixture) {
			if _, err := f.svc.ApprovedWorkflows(context.Background(), f.key); err != nil {
				f.t.Fatal(err)
			}
		},
		// GitHub #74 (2026-10-05 audit): list_recipes skipped coverOnRead.
		"list_recipes": func(f *agentAdminFixture) {
			if _, err := f.svc.ListRecipesJSON(context.Background(), f.key); err != nil {
				f.t.Fatal(err)
			}
		},
		"define_workflow": func(f *agentAdminFixture) {
			slugs++
			f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "aa", Slug: []string{"wa", "wb", "wc"}[slugs%3],
				Steps: []agentadmin.StepInput{{Name: "s", Role: "worker", Instructions: "x"}}, Egress: egress})
		},
	}
	for name, verb := range verbs {
		t.Run(name, func(t *testing.T) {
			f := newAgentAdminFixture(t)
			f.forceOver(8)
			if n := len(f.covers()); n != 0 {
				t.Fatalf("%d cover requests before any verb", n)
			}
			verb(f)
			c := f.onePendingCover("after the first verb")
			if c.Sentence != coverSentence("10", "8") {
				t.Fatalf("sentence %q", c.Sentence)
			}
			g := c.pl.Grant
			if !g.Cover || g.AddsUSD == nil || *g.AddsUSD != 0 || g.MaxTotalUSD == nil || *g.MaxTotalUSD != 10 || len(c.pl.Locks) != 0 {
				t.Fatalf("cover grant %+v locks %v", g, c.pl.Locks)
			}
			p, err := f.c.repos.Proposals.GetByID(context.Background(), c.pl.ProposalID)
			if err != nil {
				t.Fatal(err)
			}
			if p.ApplyOps != "[]" || p.Status != persistence.ProposalStatusDraft {
				t.Fatalf("cover proposal ops %q status %s", p.ApplyOps, p.Status)
			}
			verb(f)
			f.read()
			if n := len(f.covers()); n != 1 {
				t.Fatalf("%d cover requests after three verbs, want 1", n)
			}
		})
	}
}

// Two concurrent verbs file one cover request: filing decides from the state
// loaded under the verb's lock (2b25 F4).
func TestAgentAdmin_ConcurrentVerbsFileOneCover(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.forceOver(8)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = f.svc.ListSetupJSON(context.Background(), f.key)
			} else {
				raw, _ := json.Marshal(agentadmin.SetBudgetInput{Project: "aa", MonthlyUSD: 3})
				_, err = f.svc.Do(context.Background(), f.key, agentadmin.VerbSetBudget, raw)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.covers()); n != 1 {
		t.Fatalf("%d cover requests after concurrent verbs, want 1", n)
	}
}

// While over, a change that raises the sum is refused naming the cover
// request, and the first refused verb names the request it filed itself
// (round 3 F7). Non-spending widenings and lowerings proceed.
func TestAgentAdmin_OverCeilingBlocksSpendingNamingTheCover(t *testing.T) {
	egress := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"total":{"type":"number"}}}`)
	f := newAgentAdminFixture(t)
	f.forceOver(8)
	res := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"})
	c := f.onePendingCover("after the first verb")
	if res.Effect != agentadmin.EffectRefused || !strings.Contains(res.Reason, "/ui/approve/"+c.ID) {
		t.Fatalf("create_project while over: %+v (cover %s)", res, c.ID)
	}
	raise := f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "aa", MonthlyUSD: 3})
	if raise.Effect != agentadmin.EffectRefused || !strings.Contains(raise.Reason, "/ui/approve/"+c.ID) {
		t.Fatalf("a budget raise while over: %+v", raise)
	}
	wf := f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "aa", Slug: "spend",
		Steps: []agentadmin.StepInput{{Name: "s", Role: "worker", Instructions: "x"}}, Egress: egress})
	if wf.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("a workflow definition was blocked by the budget: %+v", wf)
	}
	if low := f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "bb", MonthlyUSD: 1}); low.Effect != agentadmin.EffectApplied {
		t.Fatalf("a lowering was blocked: %+v", low)
	}
	if f.c.Registry.GetProject("hermes--xa") != nil {
		t.Fatal("the refused creation applied")
	}
	if n := len(f.covers()); n != 1 {
		t.Fatalf("%d cover requests, want 1", n)
	}
}

// Approving at an unchanged sum sets the ceiling to the sum, marks the
// no-ops proposal applied, and files nothing new.
func TestAgentAdmin_CoverApprovedAtUnchangedSumSetsTheCeiling(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.forceOver(8)
	f.read()
	c := f.onePendingCover("after a read")
	if err := f.decide(c.ID, true); err != nil {
		t.Fatalf("approving the cover request: %v", err)
	}
	if v := f.assertCovered("after the cover"); v.BudgetUSD != 10 || v.CeilingUSD != 10 {
		t.Fatalf("sum $%v ceiling $%v, want $10", v.BudgetUSD, v.CeilingUSD)
	}
	if p, _ := f.c.repos.Proposals.GetByID(context.Background(), c.pl.ProposalID); p.Status != persistence.ProposalStatusApplied {
		t.Fatalf("cover proposal is %s, want applied", p.Status)
	}
	if b, _ := f.c.repos.AgentGrants.GetCeiling(context.Background(), "hermes"); b.ApprovedByDevice != f.device.ID {
		t.Fatalf("ceiling approved by %q", b.ApprovedByDevice)
	}
	f.read()
	if n := len(f.covers()); n != 1 {
		t.Fatalf("%d cover requests after the approval, want 1", n)
	}
	// Idempotent: the re-apply loop running the effect again changes nothing.
	row, _ := f.c.repos.ApproverDevices.GetRequest(context.Background(), c.ID)
	if err := f.svc.applyApproved(context.Background(), *row); err != nil {
		t.Fatalf("re-running the cover effect: %v", err)
	}
	if v := f.assertCovered("after the re-run"); v.CeilingUSD != 10 {
		t.Fatalf("ceiling $%v", v.CeilingUSD)
	}
}

// A sum that fell while the request waited: still above the ceiling, the
// request fails and is re-filed with the true sum; within it, the request
// fails as "nothing changed" and nothing is filed.
func TestAgentAdmin_CoverWhoseSumFellWhileWaiting(t *testing.T) {
	t.Run("still over: re-filed", func(t *testing.T) {
		f := newAgentAdminFixture(t)
		f.forceOver(7)
		f.read()
		c := f.onePendingCover("after a read")
		f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "aa", MonthlyUSD: 1}) // $9
		if err := f.decide(c.ID, true); err == nil {
			t.Fatal("a cover request whose sum fell applied")
		}
		next := f.onePendingCover("after the failed approval")
		if next.pl.ReRenderOf != c.ID || *next.pl.Grant.MaxTotalUSD != 9 || next.Sentence != coverSentence("9", "7") {
			t.Fatalf("re-filed cover %+v sentence %q", next.pl, next.Sentence)
		}
		failed, _ := f.c.repos.ApproverDevices.GetRequest(context.Background(), c.ID)
		if !strings.Contains(failed.ApplyError, next.ID) || !strings.Contains(failed.ApplyError, "$9") {
			t.Fatalf("failure reason %q", failed.ApplyError)
		}
		if v := f.setup(); v.CeilingUSD != 7 {
			t.Fatalf("ceiling $%v, want $7 unchanged", v.CeilingUSD)
		}
		if err := f.decide(next.ID, true); err != nil {
			t.Fatal(err)
		}
		if v := f.assertCovered("after the re-filed cover"); v.CeilingUSD != 9 {
			t.Fatalf("ceiling $%v, want $9", v.CeilingUSD)
		}
	})
	t.Run("within: nothing changed", func(t *testing.T) {
		f := newAgentAdminFixture(t)
		f.forceOver(8)
		f.read()
		c := f.onePendingCover("after a read")
		f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "aa", MonthlyUSD: 1})
		f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "bb", MonthlyUSD: 1}) // $8
		err := f.decide(c.ID, true)
		if err == nil || !strings.Contains(err.Error(), "nothing changed") {
			t.Fatalf("approving a moot cover: %v", err)
		}
		if n := len(f.pendingCovers()); n != 0 {
			t.Fatalf("%d cover requests waiting after a moot one, want 0", n)
		}
		if v := f.assertCovered("after the moot cover"); v.CeilingUSD != 8 {
			t.Fatalf("ceiling $%v, want $8", v.CeilingUSD)
		}
		if p, _ := f.c.repos.Proposals.GetByID(context.Background(), c.pl.ProposalID); p.Status != persistence.ProposalStatusRejected {
			t.Fatalf("moot cover proposal is %s", p.Status)
		}
	})
}

// The "higher" branch (round 3 F8): the sum rose while the cover request
// waited (here: a raise applied while an operator had lifted the limit, then
// the limit lowered again). Approving it fails and re-files at the true sum;
// it never sets a limit below the sum.
func TestAgentAdmin_CoverWhoseSumRoseWhileWaitingIsReFiled(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.forceOver(8)
	f.read()
	c := f.onePendingCover("after a read")
	f.setCeiling(20)
	f.approve(f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "aa", MonthlyUSD: 4})) // $12
	f.setCeiling(8)
	if err := f.decide(c.ID, true); err == nil {
		t.Fatal("a cover request whose sum rose applied")
	}
	next := f.onePendingCover("after the failed approval")
	if next.pl.ReRenderOf != c.ID || next.Sentence != coverSentence("12", "8") {
		t.Fatalf("re-filed cover %+v %q", next.pl, next.Sentence)
	}
	if v := f.setup(); v.CeilingUSD != 8 {
		t.Fatalf("ceiling $%v, want $8 unchanged", v.CeilingUSD)
	}
}

// After a rejection no new cover request for 24 hours, read from the
// approval table (durable, round 3 F6); the block holds meanwhile; filing
// resumes after the 24 hours if still over.
func TestAgentAdmin_RejectedCoverCoolsDownFor24Hours(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.forceOver(8)
	f.read()
	c := f.onePendingCover("after a read")
	if err := f.decide(c.ID, false); err != nil {
		t.Fatal(err)
	}
	f.read()
	res := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"})
	if res.Effect != agentadmin.EffectRefused || strings.Contains(res.Reason, "/ui/approve/") {
		t.Fatalf("create_project during the cooldown: %+v", res)
	}
	if n := len(f.covers()); n != 1 {
		t.Fatalf("%d cover requests during the cooldown, want 1 (the rejected one)", n)
	}
	if b := f.budget(); b.TotalUSD != 10 || b.CeilingUSD != 8 || b.CoverRequestID != "" {
		t.Fatalf("doctor source during the cooldown %+v", b)
	}
	start := time.Now()
	f.svc.now = func() time.Time { return start.Add(23 * time.Hour) }
	f.read()
	if n := len(f.covers()); n != 1 {
		t.Fatalf("a cover request was filed 23 hours after a rejection")
	}
	f.svc.now = func() time.Time { return start.Add(25 * time.Hour) }
	f.read()
	next := f.onePendingCover("25 hours after the rejection")
	if next.ID == c.ID || next.Sentence != coverSentence("10", "8") {
		t.Fatalf("after the cooldown %+v %q", next.ID, next.Sentence)
	}
}

// An agent cannot file or shape a cover request: no verb takes the flag or
// the figure, and a non-cover grant carrying a max_total_usd equal to the sum
// keeps the §18.4 rule (it never raises the ceiling to the sum).
func TestAgentAdmin_AgentCannotFileOrShapeACover(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.fill("aa", "bb", "cc", "dd", "ee")
	for verb, raw := range map[string]string{
		agentadmin.VerbCreateProject: `{"slug":"xa","purpose":"More","cover":true,"max_total_usd":1000}`,
		agentadmin.VerbSetBudget:     `{"project":"aa","monthly_usd":4,"cover":true,"max_total_usd":1000}`,
	} {
		res, err := f.svc.Do(context.Background(), f.key, verb, json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		if res.Effect != agentadmin.EffectRefused {
			t.Fatalf("%s accepted a cover field: %+v", verb, res)
		}
	}
	if n := len(f.covers()); n != 0 {
		t.Fatalf("an agent verb filed %d cover requests", n)
	}
	f.setCeiling(8)
	egress := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"total":{"type":"number"}}}`)
	wf := f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "aa", Slug: "spend",
		Steps: []agentadmin.StepInput{{Name: "s", Role: "worker", Instructions: "x"}}, Egress: egress})
	row, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), requestIDOf(wf))
	if err != nil {
		t.Fatal(err)
	}
	var pl approvalPayload
	if err := json.Unmarshal(row.Rendered, &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Grant.Cover {
		t.Fatal("a rendered verb set the cover flag")
	}
	crafted := 10.0
	pl.Grant.MaxTotalUSD = &crafted
	row.Rendered, _ = json.Marshal(pl)
	row.DecidedByDevice = f.device.ID
	if err := f.svc.applyApproved(context.Background(), *row); err != nil {
		t.Fatal(err)
	}
	if v := f.setup(); v.CeilingUSD != 8 {
		t.Fatalf("a non-cover grant's crafted max_total_usd raised the ceiling to $%v", v.CeilingUSD)
	}
}

// A crash between re-filing a cover request and failing the old one leaves
// one replacement (round 3 F3): the retry finds it by re_render_of.
func TestAgentAdmin_CoverReFileCrashLeavesOneReplacement(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.forceOver(7)
	f.read()
	c := f.onePendingCover("after a read")
	f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "aa", MonthlyUSD: 1}) // $9
	realProposals := f.svc.proposals
	f.svc.proposals = &failRejectOnce{ProposalRepository: realProposals}
	_ = f.decide(c.ID, true) // the reject after the re-file "crashes"
	replacements := func() int {
		n := 0
		for _, r := range f.covers() {
			if r.pl.ReRenderOf == c.ID {
				n++
			}
		}
		return n
	}
	if n := replacements(); n != 1 {
		t.Fatalf("after a crash before the reject, %d replacements, want 1 (filed first)", n)
	}
	f.svc.proposals = realProposals
	row, _ := f.c.repos.ApproverDevices.GetRequest(context.Background(), c.ID)
	if err := f.svc.applyApproved(context.Background(), *row); err == nil {
		t.Fatal("the retried cover succeeded")
	}
	if n := replacements(); n != 1 {
		t.Fatalf("the retry filed a second replacement: %d", n)
	}
	if p, _ := f.c.repos.Proposals.GetByID(context.Background(), c.pl.ProposalID); p.Status != persistence.ProposalStatusRejected {
		t.Fatalf("after the retry the old cover proposal is %s, want rejected", p.Status)
	}
}

// failApproveOnce makes the first SetStatus(Approved) fail, so an approved
// request stays approved-unapplied for the effect tick to apply.
type failApproveOnce struct {
	persistence.ProposalRepository
	failed bool
}

func (f *failApproveOnce) SetStatus(ctx context.Context, id, status, actor string) error {
	if status == persistence.ProposalStatusApproved && !f.failed {
		f.failed = true
		return errors.New("injected: transient failure before the approve")
	}
	return f.ProposalRepository.SetStatus(ctx, id, status, actor)
}

// Round 4 (3a63 F4), combined: a cover request is rejected; during the 24
// hours a sum-raising request approved before the namespace went over is
// applied by the effect tick; no cover request is filed during the cooldown
// and the doctor names no waiting id; after it, the new cover request states
// the true, higher sum.
//
// As built, that request's own approval stated "approving raises the limit
// to $12", so the tick's apply sets the ceiling to the sum and closes the gap
// (§18.4 item 2). The namespace is over again only when the operator lowers
// the limit once more, which this test does to reach the post-cooldown leg.
func TestAgentAdmin_CoverCooldownTickAndAfter(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdminFixture(t)
	f.fill("aa", "bb", "cc", "dd", "ee") // $10 = the ceiling
	xa := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"})
	realProposals := f.svc.proposals
	f.svc.proposals = &failApproveOnce{ProposalRepository: realProposals}
	if err := f.decide(requestIDOf(xa), true); err == nil {
		t.Fatal("the injected transient failure did not leave the request unapplied")
	}
	f.svc.proposals = realProposals
	f.setCeiling(8) // over: $10 against $8
	f.read()
	c := f.onePendingCover("after a read")
	if err := f.decide(c.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := f.c.approverDeviceService().ReapplyApproved(ctx); err != nil {
		t.Fatalf("the effect tick: %v", err)
	}
	if f.c.Registry.GetProject("hermes--xa") == nil {
		t.Fatal("the pre-approved request did not apply on the tick")
	}
	f.read()
	if n := len(f.covers()); n != 1 {
		t.Fatalf("%d cover requests during the cooldown, want 1", n)
	}
	if b := f.budget(); b.CoverRequestID != "" || b.TotalUSD != 12 || b.CeilingUSD != 12 {
		t.Fatalf("doctor source after the tick %+v", b)
	}
	f.setCeiling(8) // the operator lowers the limit again: $12 against $8
	f.read()
	if n := len(f.covers()); n != 1 {
		t.Fatalf("a cover request was filed during the cooldown")
	}
	if b := f.budget(); b.CoverRequestID != "" || b.TotalUSD != 12 {
		t.Fatalf("doctor source during the cooldown %+v", b)
	}
	if res := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xb", Purpose: "More"}); res.Effect != agentadmin.EffectRefused {
		t.Fatalf("the block did not hold during the cooldown: %+v", res)
	}
	start := time.Now()
	f.svc.now = func() time.Time { return start.Add(25 * time.Hour) }
	f.read()
	next := f.onePendingCover("after the cooldown")
	if next.Sentence != coverSentence("12", "8") || *next.pl.Grant.MaxTotalUSD != 12 {
		t.Fatalf("after the cooldown %q %+v", next.Sentence, next.pl.Grant)
	}
	if b := f.budget(); b.CoverRequestID != next.ID {
		t.Fatalf("doctor source names %q, want %s", b.CoverRequestID, next.ID)
	}
}

// The doctor's source names the waiting cover request, and examines every
// agent namespace with a project.
func TestAgentAdmin_DoctorSourceNamesTheWaitingCover(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.forceOver(8)
	if b := f.budget(); b.TotalUSD != 10 || b.CeilingUSD != 8 || b.CoverRequestID != "" {
		t.Fatalf("before any verb %+v", b)
	}
	f.read()
	c := f.onePendingCover("after a read")
	if b := f.budget(); b.CoverRequestID != c.ID {
		t.Fatalf("doctor source %+v, want cover %s", b, c.ID)
	}
}
