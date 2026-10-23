package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
)

// GitHub #409 (2026-10-16, design §19.15): a recipe with two credentialed
// servers (morning-brief: mail and calendar). The person enters BOTH keys
// before approving the first server's tools, as a person on a phone does.
// The second server's tools approval was refused because the project was
// locked by the first, only logged, and never filed again: calendar stayed
// tools_pending and the workflow never became runnable. Found at review
// while fixing #402, observed at integration; this drives the real install,
// the real credential entry and the device's decisions.

// toolsRequestsFor lists the pending tools approvals, by server name.
func toolsRequestsFor(t *testing.T, f *agentAdminFixture) map[string]string {
	t.Helper()
	rows, err := f.c.repos.ApproverDevices.ListPending(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		var pl approvalPayload
		if json.Unmarshal(r.Rendered, &pl) != nil || !strings.Contains(r.Sentence, "is now connected") {
			continue
		}
		for _, g := range pl.Grant.Integrations {
			out[g.Name] = r.ID
		}
	}
	return out
}

// installMorningBriefBothKeys installs morning-brief on two hosts and enters
// both keys before any tools approval is decided.
func installMorningBriefBothKeys(t *testing.T, f *agentAdminFixture) {
	t.Helper()
	ctx := context.Background()
	mail := newFakeMCP(t, "Bearer "+credCanary, "gmail_get", "gmail_search")
	cal := newFakeMCP(t, "Bearer "+credCanary, "calendar_list_events")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "personal", Purpose: "Personal automations"})
	f.approve(f.do(agentadmin.VerbInstallRecipe, installInput("morning-brief", map[string]string{
		"mail_server_url": mail.srv.URL, "calendar_server_url": cal.srv.URL})))
	slots := f.pendingSlots()
	for _, cred := range []string{"MAIL_TOKEN", "CALENDAR_TOKEN"} {
		req, err := f.c.repos.ApproverDevices.GetRequest(ctx, slots[cred])
		if err != nil {
			t.Fatalf("%s: %v", cred, err)
		}
		if err := f.svc.enterCredential(ctx, f.device, *req, req.RenderedSHA256, []byte(credCanary)); err != nil {
			t.Fatal(err)
		}
		f.svc.bg.Wait() // mail's listing files first: the order is fixed
	}
}

func TestAgentAdmin_BothKeysFirstThenToolsApprovals(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	installMorningBriefBothKeys(t, f)

	waiting := toolsRequestsFor(t, f)
	if len(waiting) != 1 || waiting["mail"] == "" {
		t.Fatalf("after both keys, the pending tools approvals are %v; want mail's alone (one per project at a time)", waiting)
	}
	// What the agent reads meanwhile: calendar waits, and says for what.
	for _, s := range f.personal().Servers {
		if s.Name == "calendar" && (s.Status != "tools_pending" || !strings.Contains(s.Note, "filed once request "+waiting["mail"]+" is decided")) {
			t.Fatalf("calendar while mail's tools wait: %+v", s)
		}
	}

	if err := f.decide(waiting["mail"], true); err != nil {
		t.Fatal(err)
	}
	f.svc.bg.Wait()
	after := toolsRequestsFor(t, f)
	if len(after) != 1 || after["calendar"] == "" {
		t.Fatalf("after mail's tools were approved, the pending tools approvals are %v; want calendar's (GitHub #409: never filed again)", after)
	}
	if err := f.decide(after["calendar"], true); err != nil {
		t.Fatal(err)
	}
	f.svc.bg.Wait()
	if err := f.c.verifyAgentReach(ctx, "hermes--personal", "hermes--personal--morning-brief"); err != nil {
		t.Fatalf("morning-brief with both keys entered first is not runnable: %v", err)
	}
	for _, s := range f.personal().Servers {
		if s.Status != "approved" || s.Note != "" {
			t.Fatalf("server after both tools approvals: %+v", s)
		}
	}
	if got := toolsRequestsFor(t, f); len(got) != 0 {
		t.Fatalf("a tools approval was filed again after both were approved: %v", got)
	}
}

// A rejection frees the lock too: calendar's approval is filed, and mail's,
// which the person declined, is not asked again (re-entering its key asks).
func TestAgentAdmin_BothKeysFirstRejectedFirstStillFilesSecond(t *testing.T) {
	f := newAgentAdminFixture(t)
	installMorningBriefBothKeys(t, f)
	waiting := toolsRequestsFor(t, f)
	if waiting["mail"] == "" {
		t.Fatalf("pending tools approvals: %v", waiting)
	}
	if err := f.decide(waiting["mail"], false); err != nil {
		t.Fatal(err)
	}
	f.svc.bg.Wait()
	after := toolsRequestsFor(t, f)
	if len(after) != 1 || after["calendar"] == "" {
		t.Fatalf("after mail's tools were declined, the pending tools approvals are %v; want calendar's alone", after)
	}
	if err := f.decide(after["calendar"], false); err != nil {
		t.Fatal(err)
	}
	f.svc.bg.Wait()
	if got := toolsRequestsFor(t, f); len(got) != 0 {
		t.Fatalf("a declined tools approval was filed again: %v", got)
	}
}

// Design §19.15 item 3, the drop rule: a server whose own tools approval
// held the lock (its key entered again while that approval waited) is not
// asked again once the person decides that approval: they just answered
// for it. Control: without the drop, declining mail's tools files mail's
// tools again at once.
func TestAgentAdmin_WaitingOnOwnToolsApprovalIsNotAskedAgain(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	mail := newFakeMCP(t, "Bearer "+credCanary, "gmail_get", "gmail_search")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "personal", Purpose: "Personal automations"})
	f.approve(f.do(agentadmin.VerbInstallRecipe, installInput("inbox-digest", map[string]string{"mail_server_url": mail.srv.URL})))
	req, err := f.c.repos.ApproverDevices.GetRequest(ctx, f.pendingSlots()["MAIL_TOKEN"])
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.enterCredential(ctx, f.device, *req, req.RenderedSHA256, []byte(credCanary)); err != nil {
		t.Fatal(err)
	}
	f.svc.bg.Wait()
	first := toolsRequestsFor(t, f)["mail"]
	if first == "" {
		t.Fatal("no tools approval for mail")
	}
	// The same trigger a second entry of MAIL_TOKEN runs: refused by mail's
	// own waiting approval, and recorded as waiting on it.
	f.svc.afterCredentialStored(ctx, "hermes", "MAIL_TOKEN")
	for _, s := range f.personal().Servers {
		if s.Name == "mail" && !strings.Contains(s.Note, "filed once request "+first+" is decided") {
			t.Fatalf("mail while its own tools approval waits: %+v", s)
		}
	}
	if err := f.decide(first, false); err != nil {
		t.Fatal(err)
	}
	f.svc.bg.Wait()
	if got := toolsRequestsFor(t, f); len(got) != 0 {
		t.Fatalf("mail's tools were asked again right after the person declined them: %v", got)
	}
	for _, s := range f.personal().Servers {
		if s.Name == "mail" && (s.Status != "tools_pending" || s.Note != "") {
			t.Fatalf("mail after its declined tools approval: %+v", s)
		}
	}
}
