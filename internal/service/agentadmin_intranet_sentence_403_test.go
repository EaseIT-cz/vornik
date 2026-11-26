package service

// GitHub #403 (agent-administered design part 06, §6.10). Found at: review
// (the issue); observed by the approver at the device sentence. Test at:
// integration (the real service, State loaded from the daemon's config):
// agent_admin.private_api_hosts reaches the approval sentence. Without the
// loadStateExcluding wiring the renderer sees no list and the sentence says
// nothing about the intranet.

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/config"
)

func TestAgentAdmin403_TheListReachesTheApprovalSentence(t *testing.T) {
	f := newAgentAdminFixtureWith(t, func(c *config.Config) {
		c.AgentAdmin.PrivateAPIHosts = plainPrivateHosts("gitlab.corp.example")
	})
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "dev", Purpose: "Code"})
	res := f.do(agentadmin.VerbAddAPI, agentadmin.AddAPIInput{Project: "dev", Name: "gitlab",
		BaseURL: "https://gitlab.corp.example/api/v4", Methods: []string{"GET"}})
	if res.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("effect %q: %+v", res.Effect, res)
	}
	if !strings.Contains(res.Sentence, "gitlab.corp.example is on your list of intranet hosts (agent_admin.private_api_hosts)") {
		t.Fatalf("sentence: %s", res.Sentence)
	}
	res = f.do(agentadmin.VerbAddAPI, agentadmin.AddAPIInput{Project: "dev", Name: "pub",
		BaseURL: "https://api.public.example/v1", Methods: []string{"GET"}})
	if strings.Contains(res.Sentence, "private_api_hosts") {
		t.Fatalf("an unlisted host's sentence claims a grant: %s", res.Sentence)
	}
}
