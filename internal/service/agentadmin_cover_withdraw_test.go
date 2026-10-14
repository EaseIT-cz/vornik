package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #264 (2026-10-13), agent-administered design §9.2d item 6: a cover
// request (§18.4 F10) waited for the operator after the namespace's budgets
// were lowered back within the limit, asking a question whose answer was
// "nothing changed" (review 3948 F5). The daemon now withdraws it.

const coverWithdrawnPrefix = "Your assistant's projects now add up to $"

// An agent verb that lowers the sum within the limit withdraws the cover
// request in that same verb; list_my_setup reports it withdrawn, with the
// reason, and drains a withdrawn notification; the device can no longer
// decide it.
func TestAgentAdmin_CoverWithdrawnWhenAVerbLowersTheSumWithin(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.forceOver(8) // $10 against $8
	f.read()
	c := f.onePendingCover("after a read")
	f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "aa", MonthlyUSD: 1}) // $9: still over
	if got := f.onePendingCover("after a lowering still over"); got.ID != c.ID {
		t.Fatalf("a cover request still needed was replaced: %s", got.ID)
	}
	f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "bb", MonthlyUSD: 1}) // $8: within
	r, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != persistence.ApprovalWithdrawn || r.WithdrawnBy != AgentCoverActor ||
		r.WithdrawReason != "Your assistant's projects now add up to $8 a month, within the $8 limit. Nothing needs approving." {
		t.Fatalf("cover request after the sum fell within = %+v", r)
	}
	if n := len(f.pendingCovers()); n != 0 {
		t.Fatalf("%d cover requests waiting, want 0", n)
	}
	v := f.setup()
	if len(v.Withdrawn) != 1 || v.Withdrawn[0].ID != c.ID || !strings.HasPrefix(v.Withdrawn[0].Reason, coverWithdrawnPrefix) {
		t.Fatalf("list_my_setup withdrawn = %+v", v.Withdrawn)
	}
	for _, e := range v.Expired {
		if e.ID == c.ID {
			t.Fatal("a withdrawn request is listed as expired (re-propose)")
		}
	}
	notified := false
	for _, n := range v.Notifications {
		notified = notified || (n.ChangeID == c.ID && n.Status == persistence.ApprovalWithdrawn)
	}
	if !notified {
		t.Fatalf("no withdrawn notification: %+v", v.Notifications)
	}
	if err := f.decide(c.ID, true); !errors.Is(err, approverdevice.ErrNotDecidable) {
		t.Fatalf("deciding a withdrawn cover request: %v", err)
	}
	if v := f.assertCovered("after the withdrawal"); v.CeilingUSD != 8 {
		t.Fatalf("ceiling $%v, want $8 unchanged", v.CeilingUSD)
	}
}

// A sum brought within the limit by hand (here: the operator raised the
// limit) is noticed by the next admin verb, a read included. A withdrawn
// cover request starts no cooldown: going over again files a fresh one at
// the true sum.
func TestAgentAdmin_CoverWithdrawnByTheNextReadAndNoCooldownFollows(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.forceOver(8)
	f.read()
	c := f.onePendingCover("after a read")
	f.setCeiling(10)
	f.read()
	if r, _ := f.c.repos.ApproverDevices.GetRequest(context.Background(), c.ID); r.Status != persistence.ApprovalWithdrawn ||
		!strings.Contains(r.WithdrawReason, "within the $10 limit") {
		t.Fatalf("cover request after a read within the limit = %+v", r)
	}
	f.setCeiling(7)
	f.read()
	next := f.onePendingCover("over again after a withdrawal")
	if next.ID == c.ID || *next.pl.Grant.MaxTotalUSD != 10 || next.pl.ReRenderOf != "" {
		t.Fatalf("the cover request after a withdrawal = %s %+v", next.ID, next.pl)
	}
}

// An approved, unapplied cover request is not pending: it is not withdrawn,
// and its effect's moot branch ends it as before.
func TestAgentAdmin_ApprovedCoverIsNotWithdrawn(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.forceOver(8)
	f.read()
	c := f.onePendingCover("after a read")
	if err := f.c.repos.ApproverDevices.Decide(context.Background(), c.ID, c.RenderedSHA256, f.device.ID, true, c.CreatedAt); err != nil {
		t.Fatal(err)
	}
	f.setCeiling(10)
	f.read()
	if r, _ := f.c.repos.ApproverDevices.GetRequest(context.Background(), c.ID); r.Status != persistence.ApprovalApproved {
		t.Fatalf("an approved cover request was touched: %s", r.Status)
	}
}
