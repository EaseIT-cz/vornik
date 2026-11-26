package service

// GitHub #403 (agent-administered design part 06, §6.12): the operator's
// answer 1. Found at: review (§6.10 left the window between filing and
// approval open); a user observes it when a device approval applies, so the
// test sits at integration: the real service, the approver device service
// and the apply. The approval records which agent_admin.private_api_hosts
// entry granted a new destination; when a restart changed the list between
// filing and approval, the apply refuses and records nothing.

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

func (f *agentAdminFixture) fileGitLab403() (agentadmin.Result, persistence.AgentApprovalRequestRow) {
	f.t.Helper()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "dev", Purpose: "Code"})
	res := f.do(agentadmin.VerbAddAPI, agentadmin.AddAPIInput{Project: "dev", Name: "gitlab",
		BaseURL: "https://gitlab.corp.example/api/v4", Methods: []string{"GET"}})
	if res.Effect != agentadmin.EffectAwaiting {
		f.t.Fatalf("effect %q: %+v", res.Effect, res)
	}
	id := res.ApprovalURL[strings.LastIndex(res.ApprovalURL, "/")+1:]
	req, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return res, *req
}

// decide403 approves the request and returns the apply's error and the
// request as stored afterwards.
func (f *agentAdminFixture) decide403(req persistence.AgentApprovalRequestRow) (persistence.AgentApprovalRequestRow, error) {
	f.t.Helper()
	ctx := context.Background()
	err := f.c.approverDeviceService().Decide(ctx, f.device, req.ID, req.RenderedSHA256, true)
	got, gerr := f.c.repos.ApproverDevices.GetRequest(ctx, req.ID)
	if gerr != nil {
		f.t.Fatal(gerr)
	}
	return *got, err
}

func (f *agentAdminFixture) assertNotApplied403(res agentadmin.Result, req persistence.AgentApprovalRequestRow, decideErr error) {
	f.t.Helper()
	if decideErr == nil {
		f.t.Fatal("an approval whose intranet grant changed applied")
	}
	if !strings.Contains(req.ApplyError, "the operator's list of intranet hosts changed after you were asked") {
		f.t.Fatalf("apply error %q", req.ApplyError)
	}
	p, err := f.c.repos.Proposals.GetByID(context.Background(), res.ChangeID)
	if err != nil || p.Status != persistence.ProposalStatusRejected {
		f.t.Fatalf("proposal: %+v, %v", p, err)
	}
	if a, err := f.c.repos.AgentGrants.GetIntegration(context.Background(), "hermes--dev", "gitlab"); err == nil && a.RemovedAt == nil {
		f.t.Fatalf("an integration was recorded: %+v", a)
	}
}

// Filed with no list (as every request pending at the upgrade was: no
// released daemon recorded a grant), approved after a restart that listed
// the host: refused.
func TestAgentAdmin403_ListAddedBeforeApprovalIsRefused(t *testing.T) {
	f := newAgentAdminFixture(t)
	res, req := f.fileGitLab403()
	// The ungranted document has no private_reach at all: byte for byte the
	// shape of a request filed before #403 (omitempty).
	if strings.Contains(string(req.Rendered), "private_reach") {
		t.Fatalf("an ungranted request records private_reach: %s", req.Rendered)
	}
	f.c.Config.AgentAdmin.PrivateAPIHosts = plainPrivateHosts("gitlab.corp.example") // the restart's effect
	got, err := f.decide403(req)
	f.assertNotApplied403(res, got, err)
}

func TestAgentAdmin403_ListRemovedBeforeApprovalIsRefused(t *testing.T) {
	f := newAgentAdminFixtureWith(t, func(c *config.Config) {
		c.AgentAdmin.PrivateAPIHosts = plainPrivateHosts("gitlab.corp.example")
	})
	res, req := f.fileGitLab403()
	if !strings.Contains(string(req.Rendered), `"private_reach":"gitlab.corp.example"`) {
		t.Fatalf("the grant is not recorded: %s", req.Rendered)
	}
	f.c.Config.AgentAdmin.PrivateAPIHosts = nil
	got, err := f.decide403(req)
	f.assertNotApplied403(res, got, err)
}

func TestAgentAdmin403_PinsAddedBeforeApprovalAreRefused(t *testing.T) {
	f := newAgentAdminFixtureWith(t, func(c *config.Config) {
		c.AgentAdmin.PrivateAPIHosts = plainPrivateHosts("gitlab.corp.example")
	})
	res, req := f.fileGitLab403()
	f.c.Config.AgentAdmin.PrivateAPIHosts = []config.PrivateAPIHost{{Host: "gitlab.corp.example", Addresses: []string{"10.0.0.0/24"}}}
	got, err := f.decide403(req)
	f.assertNotApplied403(res, got, err)
}

func TestAgentAdmin403_SameListApplies(t *testing.T) {
	f := newAgentAdminFixtureWith(t, func(c *config.Config) {
		c.AgentAdmin.PrivateAPIHosts = plainPrivateHosts("gitlab.corp.example")
	})
	res, _ := f.fileGitLab403()
	f.approve(res)
	if a, err := f.c.repos.AgentGrants.GetIntegration(context.Background(), "hermes--dev", "gitlab"); err != nil || a.RemovedAt != nil {
		t.Fatalf("the approved integration is not recorded: %+v, %v", a, err)
	}
}

// Answer 5: an entry naming projects serves those only; another project's
// sentence names no grant and its dial client gets no entries.
func TestAgentAdmin403_AnEntryServesItsProjectsOnly(t *testing.T) {
	f := newAgentAdminFixtureWith(t, func(c *config.Config) {
		c.AgentAdmin.PrivateAPIHosts = []config.PrivateAPIHost{{Host: "gitlab.corp.example", Projects: []string{"hermes--other"}}}
	})
	res, req := f.fileGitLab403()
	if strings.Contains(res.Sentence, "private_api_hosts") || strings.Contains(string(req.Rendered), "private_reach") {
		t.Fatalf("a project the entry does not serve got the grant: %s", res.Sentence)
	}
	if got := f.c.agentPrivateHostsFor("hermes--dev"); len(got) != 0 {
		t.Errorf("hermes--dev's dial entries: %v", got)
	}
	if got := f.c.agentPrivateHostsFor("hermes--other"); len(got) != 1 || got[0].Host != "gitlab.corp.example" {
		t.Errorf("hermes--other's dial entries: %v", got)
	}
}
