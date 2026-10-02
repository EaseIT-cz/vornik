package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
)

func setupOf(t *testing.T, f *agentAdminFixture) SetupView {
	t.Helper()
	v, err := f.svc.ListSetup(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func projectView(v SetupView, id string) *SetupProject {
	for i := range v.Projects {
		if v.Projects[i].ID == id {
			return &v.Projects[i]
		}
	}
	return nil
}

// Plan P4.6: list_my_setup shows each credential's name, kind and whether
// it is set (never its value), the project's APIs, and a server's
// proposable write tools; describe_installation lists the P4 verbs as
// available. Control: the credential and API views.
func TestAgentAdmin_SetupShowsCredentialsAndAPIs(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Money"})
	f.approve(f.do(agentadmin.VerbAddAPI, agentadmin.AddAPIInput{Project: "finance", Name: "fio", BaseURL: "https://api.fio.example/v1",
		Auth: agentadmin.APIAuthInput{Credential: "FIO"}, Methods: []string{"GET"}}))
	p := projectView(setupOf(t, f), "hermes--finance")
	if p == nil || len(p.Credentials) != 1 || p.Credentials[0].Name != "FIO" || p.Credentials[0].Kind != "secret" || p.Credentials[0].Status != "missing" {
		t.Fatalf("before entry: %+v", p)
	}
	if len(p.APIs) != 1 || p.APIs[0].Name != "fio" || p.APIs[0].Status != "approved" || strings.Join(p.APIs[0].ReadMethods, ",") != "GET" {
		t.Fatalf("apis %+v", p.APIs)
	}
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "finance", Name: "FIO", Purpose: "x", Kind: "secret"})
	slot, _ := f.c.repos.ApproverDevices.GetRequest(ctx, requestIDOf(res))
	if err := f.svc.enterCredential(ctx, f.device, *slot, slot.RenderedSHA256, []byte(credCanary)); err != nil {
		t.Fatal(err)
	}
	f.svc.bg.Wait()
	v := setupOf(t, f)
	p = projectView(v, "hermes--finance")
	if p.Credentials[0].Status != "set" || p.Credentials[0].SetAt == nil {
		t.Fatalf("after entry: %+v", p.Credentials)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), credCanary) {
		t.Fatal("list_my_setup carries the value")
	}
	caps, err := f.svc.Describe(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(caps.EgressScan, "names the field") {
		t.Errorf("describe_installation does not state the egress scan: %q", caps.EgressScan)
	}
	// Design §17: a schedule is a widening the user approves.
	if !strings.Contains(strings.Join(caps.NeedsApproval, " "), "schedule") {
		t.Errorf("needs_approval does not name schedules: %q", caps.NeedsApproval)
	}
	if !strings.HasPrefix(caps.Writes, "off") {
		t.Errorf("writes with broker.writes unset: %q", caps.Writes)
	}
	all := strings.Join(caps.Verbs, ",")
	for _, v := range []string{agentadmin.VerbAddAPI, agentadmin.VerbRequestCredential} {
		if !strings.Contains(all, v) {
			t.Errorf("describe_installation does not list %s", v)
		}
	}
	for _, n := range caps.NotYet {
		if n == "add_api" || n == "request_credential" || n == "OAuth sign-in" || n == "proposed writes" || n == "schedules" {
			t.Errorf("%q is still listed as not available", n)
		}
	}
}

// Plan P6.4: describe_installation states the harness class of the key's
// client kind and its §3 claims, from the one predicate connect also uses
// (amendment F8); an unknown kind reads as shell-capable. Control:
// Describe's Harness row.
func TestAgentAdmin_DescribeStatesTheHarness(t *testing.T) {
	f := newAgentAdminFixture(t)
	for kind, want := range map[string]string{"hermes": agentadmin.HarnessMCPOnly, "claude-desktop": agentadmin.HarnessMCPOnly,
		"codex": agentadmin.HarnessShellCapable, "claude-code": agentadmin.HarnessShellCapable, "mystery": agentadmin.HarnessShellCapable} {
		key := *f.key
		key.ClientKind = kind
		caps, err := f.svc.Describe(context.Background(), &key)
		if err != nil {
			t.Fatal(err)
		}
		if caps.Harness.ClientKind != kind || caps.Harness.Class != want || len(caps.Harness.Claims) == 0 {
			t.Errorf("%s: %+v, want class %s", kind, caps.Harness, want)
		}
		// Review 20261002-6f6b F3: the guidance reaches every harness here,
		// whether or not it surfaces the initialize instructions.
		if caps.HowToWork != agentadmin.AdminGuidance() {
			t.Errorf("%s: how_to_work is not the admin guidance", kind)
		}
	}
}
