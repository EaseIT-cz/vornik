package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// Agent-administered Vornik design §19 (recipes), the service half: one
// approval, then the credential requests; the credential-completeness gate;
// narrowings recorded and counted; what list_my_setup says next.

func installInput(recipe string, vars map[string]string) agentadmin.InstallRecipeInput {
	in := agentadmin.InstallRecipeInput{Recipe: recipe, Project: "personal", Variables: map[string]json.RawMessage{}}
	for k, v := range vars {
		raw, _ := json.Marshal(v)
		in.Variables[k] = raw
	}
	return in
}

// pendingSlots lists the namespace's pending credential requests by name.
func (f *agentAdminFixture) pendingSlots() map[string]string {
	f.t.Helper()
	rows, err := f.c.repos.ApproverDevices.ListPending(context.Background(), time.Now().UTC())
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		if r.Kind != persistence.ApprovalKindCredentialSlot {
			continue
		}
		var pl approvalPayload
		if json.Unmarshal(r.Rendered, &pl) == nil && pl.Slot != nil {
			out[pl.Slot.Name] = r.ID
		}
	}
	return out
}

func (f *agentAdminFixture) personal() SetupProject {
	const id = "hermes--personal"
	f.t.Helper()
	for _, p := range f.setup().Projects {
		if p.ID == id {
			return p
		}
	}
	f.t.Fatalf("no project %s", id)
	return SetupProject{}
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

// Items 2 and 6: the install is one approval; the credential request
// follows it automatically; until the credential is set the workflow is
// installed but not runnable (SETUP_INCOMPLETE), list_my_setup names the
// next step, and the credential-first server's tools are approved after it
// is set, exactly the recipe's. Removing the credential stops it again.
func TestAgentAdmin_InstallRecipeEndToEnd(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	mail := newFakeMCP(t, "Bearer "+credCanary, "gmail_get", "gmail_search", "gmail_send")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "personal", Purpose: "Personal automations"})

	res := f.do(agentadmin.VerbInstallRecipe, installInput("inbox-digest", map[string]string{"mail_server_url": mail.srv.URL}))
	if res.Effect != agentadmin.EffectAwaiting || !strings.Contains(res.Sentence, mail.srv.URL) {
		t.Fatalf("install: %+v", res)
	}
	if got := f.pendingSlots(); len(got) != 0 {
		t.Fatalf("a credential was requested before the install was approved: %v", got)
	}
	if len(mail.seen()) != 0 {
		t.Fatal("the credentialed server was contacted before approval")
	}
	f.approve(res)
	const wid = "hermes--personal--inbox-digest"
	if f.c.Registry.GetWorkflow(wid) == nil || f.c.Registry.GetSwarm("hermes--personal") == nil {
		t.Fatal("the recipe did not load")
	}
	slots := f.pendingSlots()
	if len(slots) != 1 || slots["MAIL_TOKEN"] == "" {
		t.Fatalf("credential requests after the approval: %v", slots)
	}
	if p := f.personal(); !hasLine(p.NextSteps, "enter MAIL_TOKEN on your phone") {
		t.Fatalf("next steps: %v", p.NextSteps)
	}

	var incomplete *agentadmin.SetupIncompleteError
	if err := f.c.verifyAgentReach(ctx, "hermes--personal", wid); !errors.As(err, &incomplete) || incomplete.Credential != "MAIL_TOKEN" {
		t.Fatalf("a workflow without its credential is runnable: %v", err)
	}

	req, _ := f.c.repos.ApproverDevices.GetRequest(ctx, slots["MAIL_TOKEN"])
	if err := f.svc.enterCredential(ctx, f.device, *req, req.RenderedSHA256, []byte(credCanary)); err != nil {
		t.Fatal(err)
	}
	tools := pendingToolsRequest(t, f)
	if !strings.Contains(tools.Sentence, "gmail_get, gmail_search") || strings.Contains(tools.Sentence, "gmail_send") {
		t.Fatalf("the tools approval is not the recipe's tools: %s", tools.Sentence)
	}
	if err := f.decide(tools.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.c.verifyAgentReach(ctx, "hermes--personal", wid); err != nil {
		t.Fatalf("the installed, credentialed, approved recipe is not runnable: %v", err)
	}
	if p := f.personal(); len(p.NextSteps) != 0 {
		t.Fatalf("next steps after setup: %v", p.NextSteps)
	}

	// A credential removed while a task waits: the gate stops it again.
	if err := f.c.currentSecretSource().Store.Delete(ctx, "hermes", "MAIL_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := f.c.verifyAgentReach(ctx, "hermes--personal", wid); !errors.As(err, &incomplete) {
		t.Fatalf("a removed credential did not stop the workflow: %v", err)
	}

	// The same install again files nothing.
	again := f.do(agentadmin.VerbInstallRecipe, installInput("inbox-digest", map[string]string{"mail_server_url": mail.srv.URL}))
	if again.Effect != agentadmin.EffectApplied || again.ChangeID != "" {
		t.Fatalf("a reinstall is not inert: %+v", again)
	}
	if got := f.pendingSlots(); len(got) != 0 {
		t.Fatalf("a reinstall requested credentials: %v", got)
	}
}

// §19.9 F1, §19.11 F5: one request per credential that is not set; a
// credential already entered is not asked for again.
func TestAgentAdmin_InstallRecipeAsksOnlyForMissingCredentials(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "personal", Purpose: "Personal automations"})
	st, err := f.c.secretStoreForWrite()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "hermes", "MAIL_TOKEN", "secret", []byte(credCanary), f.device.ID); err != nil {
		t.Fatal(err)
	}
	res := f.do(agentadmin.VerbInstallRecipe, installInput("morning-brief", map[string]string{
		"mail_server_url": "https://mail.invalid/mcp", "calendar_server_url": "https://cal.invalid/mcp"}))
	f.approve(res)
	if got := f.pendingSlots(); len(got) != 1 || got["CALENDAR_TOKEN"] == "" {
		t.Fatalf("credential requests: %v", got)
	}
}

// §19.7 F2: a declined credential leaves the install in place, and
// list_my_setup says so with what to do.
func TestAgentAdmin_InstallRecipeDeclinedCredential(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "personal", Purpose: "Personal automations"})
	f.approve(f.do(agentadmin.VerbInstallRecipe, installInput("agenda", map[string]string{"calendar_server_url": "https://cal.invalid/mcp"})))
	slot := f.pendingSlots()["CALENDAR_TOKEN"]
	if slot == "" {
		t.Fatal("no credential request")
	}
	if err := f.decide(slot, false); err != nil {
		t.Fatal(err)
	}
	if f.c.Registry.GetWorkflow("hermes--personal--agenda") == nil {
		t.Fatal("a declined credential removed the install")
	}
	if p := f.personal(); !hasLine(p.NextSteps, "CALENDAR_TOKEN was declined") {
		t.Fatalf("next steps: %v", p.NextSteps)
	}
}

// writeProbe puts a test recipe into the fixture's deployed catalogue and
// rebuilds the renderer from it.
func (f *agentAdminFixture) writeProbe(version int, readTools string) {
	f.t.Helper()
	dir := filepath.Join(f.cfgDir, "configs", "agent-templates", "recipes", "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	var tools []string
	for _, t := range strings.Split(readTools, ", ") {
		tools = append(tools, "mcp__src__"+t)
	}
	body := `name: probe
version: ` + string(rune('0'+version)) + `
envelope_version: 1
title: "Probe"
summary: "Probe summary."
returns: "what the probe found"
variables:
  server_url: {type: url, help: "the server"}
needs:
  - {name: src, reads: "your source", url: "{{server_url}}", read_tools: [` + readTools + `]}
team:
  - {name: reader, instructions: "Read the source.", tools: [file_write, ` + strings.Join(tools, ", ") + `]}
workflow:
  slug: probe
  purpose: "Probe the source"
  steps:
    - {name: look, role: reader, instructions: "Look at the source."}
  egress:
    item_properties:
      note: {type: string, maxLength: 100}
schedule_default: null
`
	if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	r, err := agentadmin.NewRenderer(os.DirFS(filepath.Join(f.cfgDir, "configs", "agent-templates")))
	if err != nil {
		f.t.Fatal(err)
	}
	f.svc.renderer = r
}

// §19.9 F2, §19.10 F9, §19.11: tools only removed are a narrowing applied
// without approval, recorded on the approval and the change log, and
// counted; an unauthenticated server is listed before approval and one
// lacking a recipe tool refuses the install (the unauthenticated tool
// check).
func TestAgentAdmin_RecipeNarrowingIsRecordedAndCounted(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	src := newFakeMCP(t, "", "look", "peek")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "personal", Purpose: "Personal automations"})

	f.writeProbe(1, "look, peek, scan")
	if res := f.do(agentadmin.VerbInstallRecipe, installInput("probe", map[string]string{"server_url": src.srv.URL})); res.Effect != agentadmin.EffectRefused || !strings.Contains(res.Reason, "does not offer scan") {
		t.Fatalf("an unauthenticated server without a recipe tool: %+v", res)
	}

	f.writeProbe(1, "look, peek")
	f.approve(f.do(agentadmin.VerbInstallRecipe, installInput("probe", map[string]string{"server_url": src.srv.URL})))
	a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--personal", "src")
	if err != nil || a.ReadPending || strings.Join(a.ReadTools, ",") != "look,peek" {
		t.Fatalf("approval: %+v %v", a, err)
	}

	before := testutil.ToFloat64(agentAdminNarrowings.WithLabelValues("recipe_tools"))
	f.writeProbe(2, "look")
	res := f.do(agentadmin.VerbInstallRecipe, installInput("probe", map[string]string{"server_url": src.srv.URL}))
	if res.Effect != agentadmin.EffectApplied || !strings.Contains(res.Sentence, "no longer uses peek") {
		t.Fatalf("narrowing: %+v", res)
	}
	if got := testutil.ToFloat64(agentAdminNarrowings.WithLabelValues("recipe_tools")) - before; got != 1 {
		t.Fatalf("narrowings counted: %v", got)
	}
	a, _ = f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--personal", "src")
	if strings.Join(a.ReadTools, ",") != "look" || a.ApprovedByDevice != f.device.ID {
		t.Fatalf("the narrowing is not on the approval: %+v", a)
	}
	// Review 20261003-e379 F3: the execution gate judges the approval table,
	// not the rendered allowed_tools, so the removed tool is refused and the
	// kept one admitted.
	if why := agentadmin.ToolApprovalRefusal(ctx, f.c.repos.AgentGrants, "hermes--personal", "mcp__src__peek", false); why == "" {
		t.Fatal("the gate admits a tool the narrowing removed")
	}
	if why := agentadmin.ToolApprovalRefusal(ctx, f.c.repos.AgentGrants, "hermes--personal", "mcp__src__look", false); why != "" {
		t.Fatalf("the gate refuses a kept tool: %s", why)
	}
	p, err := f.c.repos.Proposals.GetByID(ctx, res.ChangeID)
	if err != nil || p.Status != persistence.ProposalStatusApplied || !strings.Contains(p.Title, "no longer uses peek") {
		t.Fatalf("the change log does not record it: %+v %v", p, err)
	}
}

// §19.8 F6: a credential-first server that does not offer a recipe tool
// gets no tools approval; list_my_setup names the missing tools.
func TestAgentAdmin_RecipeToolsGapIsShown(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	mail := newFakeMCP(t, "Bearer "+credCanary, "gmail_search")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "personal", Purpose: "Personal automations"})
	f.approve(f.do(agentadmin.VerbInstallRecipe, installInput("inbox-digest", map[string]string{"mail_server_url": mail.srv.URL})))
	req, _ := f.c.repos.ApproverDevices.GetRequest(ctx, f.pendingSlots()["MAIL_TOKEN"])
	if err := f.svc.enterCredential(ctx, f.device, *req, req.RenderedSHA256, []byte(credCanary)); err != nil {
		t.Fatal(err)
	}
	f.svc.bg.Wait()
	rows, _ := f.c.repos.ApproverDevices.ListPending(ctx, time.Now().UTC())
	for _, r := range rows {
		if strings.Contains(r.Sentence, "is now connected") {
			t.Fatalf("a tools approval was filed for a server without the recipe's tools: %s", r.Sentence)
		}
	}
	var note string
	for _, s := range f.personal().Servers {
		if s.Name == "mail" {
			note = s.Note
		}
	}
	if !strings.Contains(note, "does not offer gmail_get") {
		t.Fatalf("list_my_setup does not name the gap: %q", note)
	}
}

// §19.2: the verbs are described, and list_recipes says where a recipe is
// installed.
func TestAgentAdmin_RecipeVerbsAreDescribedAndListed(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	caps, err := f.svc.Describe(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{agentadmin.VerbListRecipes, agentadmin.VerbInstallRecipe} {
		if !hasLine(caps.Verbs, v) {
			t.Fatalf("describe_installation does not list %s: %v", v, caps.Verbs)
		}
	}
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "personal", Purpose: "Personal automations"})
	f.approve(f.do(agentadmin.VerbInstallRecipe, installInput("agenda", map[string]string{"calendar_server_url": "https://cal.invalid/mcp"})))
	views, err := f.svc.ListRecipes(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if v.Name == "agenda" && (len(v.Installed) != 1 || v.Installed[0].Project != "hermes--personal") {
			t.Fatalf("agenda installed: %+v", v.Installed)
		}
	}
}
