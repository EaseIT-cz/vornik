package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #314 (2026-10-13): a paired device installs a vetted read-only
// connector in one confirm (agent-administered design §19.14.2, build
// decisions §19.14.8 item 5). The tests named in §19.14.6, service half.

// connectorFixture is the agent admin fixture with the shipped agenda
// recipe made a connector whose vetted server is a fake MCP server, and a
// project "personal" in the hermes namespace.
type connectorFixture struct {
	*agentAdminFixture
	cal   *fakeMCP
	pages approverdevice.ConnectorPages
}

func newConnectorFixture(t *testing.T, edit func(recipe string) string) *connectorFixture {
	t.Helper()
	f := newAgentAdminFixture(t)
	cal := newFakeMCP(t, "Bearer "+credCanary, "calendar_list_events")
	path := filepath.Join(f.cfgDir, "configs", "agent-templates", "recipes", "agenda", "recipe.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	recipe := strings.TrimRight(string(raw), "\n") + "\n" + `connector:
  title: "Calendar (read)"
  servers:
    calendar: {url: "` + cal.srv.URL + `"}
  vetted: {by: "release reviewer", at: "2026-10-13", recipe_version: 1}
`
	if edit != nil {
		recipe = edit(recipe)
	}
	if err := os.WriteFile(path, []byte(recipe), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := agentadmin.NewRenderer(os.DirFS(filepath.Join(f.cfgDir, "configs", "agent-templates")))
	if err != nil {
		t.Fatal(err)
	}
	f.svc.renderer = r
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "personal", Purpose: "Personal automations"})
	return &connectorFixture{agentAdminFixture: f, cal: cal, pages: connectorPages{s: f.svc}}
}

func (f *connectorFixture) choice(account string) approverdevice.ConnectorChoice {
	return approverdevice.ConnectorChoice{Namespace: "hermes", Project: "personal", Account: account}
}

// confirm previews then confirms, as the page does.
func (f *connectorFixture) confirm(ch approverdevice.ConnectorChoice, creds map[string]string) approverdevice.ConnectorOutcome {
	f.t.Helper()
	ctx := context.Background()
	pv, err := f.pages.Preview(ctx, "agenda", ch)
	if err != nil || pv.Refused != "" {
		f.t.Fatalf("preview: %+v, %v", pv, err)
	}
	values := map[string][]byte{}
	for k, v := range creds {
		values[k] = []byte(v)
	}
	out, err := f.pages.Confirm(ctx, f.device, "agenda", ch, pv.Shown, values)
	if err != nil {
		f.t.Fatalf("confirm: %v", err)
	}
	return out
}

func (f *connectorFixture) pendingCount() int {
	rows, err := f.c.repos.ApproverDevices.ListPending(context.Background(), time.Now().UTC())
	if err != nil {
		f.t.Fatal(err)
	}
	return len(rows)
}

// One confirm: installed, the credential stored, exactly the read tools
// approved, the connection checked, nothing left pending on the phone, and
// list_my_setup marks the workflow installed by the device.
func TestConnectorConfirm_OneSubmissionInstallsCredentialsAndTools(t *testing.T) {
	f := newConnectorFixture(t, nil)
	ctx := context.Background()
	out := f.confirm(f.choice(""), map[string]string{"CALENDAR_TOKEN": credCanary})
	if out.Refused != "" || !out.Installed {
		t.Fatalf("outcome %+v", out)
	}
	const wid = "hermes--personal--agenda"
	if err := f.c.verifyAgentReach(ctx, "hermes--personal", wid); err != nil {
		t.Fatalf("the connector is not runnable: %v", err)
	}
	a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--personal", "calendar")
	if err != nil || a.ReadPending || strings.Join(a.ReadTools, ",") != "calendar_list_events" || a.ApprovedByDevice != f.device.ID {
		t.Fatalf("integration approval %+v, %v", a, err)
	}
	if n := f.pendingCount(); n != 0 {
		t.Fatalf("%d requests left pending on the phone", n)
	}
	// Review 99c9 F2: the install filed no credential request of its own and
	// no background tools approval: exactly the device's three decisions.
	f.svc.bg.Wait()
	rows, err := f.c.repos.ApproverDevices.ListRecentByNamespace(ctx, "hermes", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, r := range rows {
		if r.Status != persistence.ApprovalApproved || r.DecidedByDevice != f.device.ID {
			t.Fatalf("request %s %s is %s by %q", r.ID, r.Kind, r.Status, r.DecidedByDevice)
		}
		kinds[r.Kind]++
	}
	if kinds[persistence.ApprovalKindCredentialSlot] != 1 || kinds[persistence.ApprovalKindWideningChange] != 2 || len(rows) != 3 {
		t.Fatalf("requests by kind %v (of %d)", kinds, len(rows))
	}
	if len(out.Checks) != 1 || out.Checks[0].Code != string(agentadmin.ConnOK) {
		t.Fatalf("checks %+v", out.Checks)
	}
	for _, auth := range f.cal.seen() {
		if auth != "Bearer "+credCanary {
			t.Fatalf("the server was called without the stored credential: %q", auth)
		}
	}
	var found bool
	for _, w := range f.personal().Workflows {
		if w.ID == wid {
			found = true
			if w.InstalledBy != "device" || !w.Approved {
				t.Fatalf("workflow view %+v", w)
			}
		}
	}
	if !found {
		t.Fatal("list_my_setup does not show the workflow")
	}
}

// The account label is in the sentence the person confirms and on what is
// installed (§22.2.5, §22.2.8).
func TestConnectorConfirm_NamesTheAccount(t *testing.T) {
	f := newConnectorFixture(t, nil)
	ctx := context.Background()
	pv, err := f.pages.Preview(ctx, "agenda", f.choice("work"))
	if err != nil || !strings.Contains(pv.Sentence, "work") || !strings.Contains(pv.Sentence, "hermes") {
		t.Fatalf("preview %+v, %v", pv, err)
	}
	if len(pv.Credentials) != 1 || pv.Credentials[0].Name != "CALENDAR_TOKEN_WORK" {
		t.Fatalf("credentials %+v", pv.Credentials)
	}
	out := f.confirm(f.choice("work"), map[string]string{"CALENDAR_TOKEN_WORK": credCanary})
	if !out.Installed || out.Refused != "" {
		t.Fatalf("outcome %+v", out)
	}
	a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--personal", "calendar-work")
	if err != nil || a.StoredAccountLabel() != "work" || a.ReadPending {
		t.Fatalf("integration %+v, %v", a, err)
	}
	if !f.c.storedCredentials(ctx, "hermes")["CALENDAR_TOKEN_WORK"] {
		t.Fatal("the account's credential was not stored")
	}
}

func TestConnectorConfirm_RefusesUnpairedDevice(t *testing.T) {
	f := newConnectorFixture(t, nil)
	ctx := context.Background()
	pv, err := f.pages.Preview(ctx, "agenda", f.choice(""))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.pages.Confirm(ctx, nil, "agenda", f.choice(""), pv.Shown, map[string][]byte{"CALENDAR_TOKEN": []byte(credCanary)})
	if !errors.Is(err, approverdevice.ErrNoDevice) {
		t.Fatalf("no device: %v", err)
	}
	if f.c.Registry.GetWorkflow("hermes--personal--agenda") != nil {
		t.Fatal("installed without a device")
	}
}

// §19.14.2 ordering invariant: install, credential store, tools approval,
// check; a failed credential store leaves the tools unapproved (the #312
// state is unreachable on this path). The step seam makes it deterministic.
func TestConnectorConfirm_OrderCredentialBeforeTools(t *testing.T) {
	f := newConnectorFixture(t, nil)
	var steps []string
	entered := 0
	f.svc.connectorStep = func(step string) error { steps = append(steps, step); return nil }
	f.svc.afterCredentialHook = func() { entered++ }
	f.confirm(f.choice(""), map[string]string{"CALENDAR_TOKEN": credCanary})
	if got := strings.Join(steps, ","); got != "install,credential,tools,check" {
		t.Fatalf("order %s", got)
	}
	// Review 99c9 F1: the agent path's background tools listing never runs.
	f.svc.bg.Wait()
	if entered != 0 {
		t.Fatalf("afterCredentialStored ran %d times on the connector path", entered)
	}

	g := newConnectorFixture(t, nil)
	g.svc.connectorStep = func(step string) error {
		if step == "credential" {
			return errors.New("injected: the store is down")
		}
		return nil
	}
	out := g.confirm(g.choice(""), map[string]string{"CALENDAR_TOKEN": credCanary})
	if !out.Installed || out.Failed != "credential" {
		t.Fatalf("outcome %+v", out)
	}
	a, err := g.c.repos.AgentGrants.GetIntegration(context.Background(), "hermes--personal", "calendar")
	if err != nil || !a.ReadPending || len(a.ReadTools) != 0 {
		t.Fatalf("tools were approved without a stored credential: %+v, %v", a, err)
	}
}

// A confirm into namespace codex is absent from hermes's setup.
func TestConnectorConfirm_OtherNamespaceUnchanged(t *testing.T) {
	f := newConnectorFixture(t, nil)
	codex := &persistence.APIKey{ID: "akey_codex", ClientKind: "codex", AgentAdmin: true, AgentNamespace: "codex"}
	raw := []byte(`{"slug":"personal","purpose":"Codex automations"}`)
	if res, err := f.svc.Do(context.Background(), codex, agentadmin.VerbCreateProject, raw); err != nil || res.Effect != agentadmin.EffectApplied {
		t.Fatalf("codex project: %+v, %v", res, err)
	}
	ch := approverdevice.ConnectorChoice{Namespace: "codex", Project: "personal"}
	pv, err := f.pages.Preview(context.Background(), "agenda", ch)
	if err != nil || !strings.Contains(pv.Sentence, "codex") {
		t.Fatalf("preview %+v, %v", pv, err)
	}
	out, err := f.pages.Confirm(context.Background(), f.device, "agenda", ch, pv.Shown, map[string][]byte{"CALENDAR_TOKEN": []byte(credCanary)})
	if err != nil || !out.Installed {
		t.Fatalf("confirm %+v, %v", out, err)
	}
	if f.c.Registry.GetWorkflow("codex--personal--agenda") == nil {
		t.Fatal("not installed in codex")
	}
	for _, w := range f.personal().Workflows {
		if strings.Contains(w.ID, "agenda") {
			t.Fatalf("hermes sees %s", w.ID)
		}
	}
	if f.c.storedCredentials(context.Background(), "hermes")["CALENDAR_TOKEN"] {
		t.Fatal("the credential went to hermes")
	}
}

// §19.14.10 (superseding §19.14.8 item 4): an OAuth connector's tools wait
// for the callback. Here the server offers no sign-in, so the sign-in
// cannot start: the install stands, read-pending, and no tool is approved.
func TestConnectorConfirm_WaitsForOAuthCallback(t *testing.T) {
	f := newConnectorFixture(t, func(r string) string {
		return strings.Replace(r, "credential: {name: CALENDAR_TOKEN, kind: secret}", "credential: {name: OAUTH_CALENDAR, kind: oauth}", 1)
	})
	ctx := context.Background()
	pv, err := f.pages.Preview(ctx, "agenda", f.choice(""))
	if err != nil || pv.Refused != "" {
		t.Fatalf("preview %+v, %v", pv, err)
	}
	out, err := f.pages.Confirm(ctx, f.device, "agenda", f.choice(""), pv.Shown, nil)
	if err != nil || !out.Installed || out.Failed != "sign-in" || out.SignInURL != "" {
		t.Fatalf("confirm %+v, %v", out, err)
	}
	a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--personal", "calendar")
	if err != nil || !a.ReadPending || len(a.ReadTools) != 0 {
		t.Fatalf("tools approved before any sign-in: %+v, %v", a, err)
	}
}

// A forged variable naming the server URL is ignored: the vetted URL is
// what is installed.
func TestConnectorConfirm_NeverAcceptsAURLOrTool(t *testing.T) {
	f := newConnectorFixture(t, nil)
	ch := f.choice("")
	ch.Variables = map[string]string{"calendar_server_url": "https://evil.example/mcp"}
	out := f.confirm(ch, map[string]string{"CALENDAR_TOKEN": credCanary})
	if !out.Installed {
		t.Fatalf("outcome %+v", out)
	}
	p := f.c.Registry.GetProject("hermes--personal")
	for _, srv := range p.MCP.Servers {
		if srv.Name == "calendar" && srv.URL != f.cal.srv.URL {
			t.Fatalf("installed URL %s", srv.URL)
		}
	}
}

// What is applied is what was shown: a stale shown value files nothing.
func TestConnectorConfirm_StaleShownFilesNothing(t *testing.T) {
	f := newConnectorFixture(t, nil)
	ctx := context.Background()
	out, err := f.pages.Confirm(ctx, f.device, "agenda", f.choice(""), "not-what-was-shown", map[string][]byte{"CALENDAR_TOKEN": []byte(credCanary)})
	if err != nil || out.Installed || out.Refused == "" || out.Sentence == "" {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	if f.c.Registry.GetWorkflow("hermes--personal--agenda") != nil || f.pendingCount() != 0 {
		t.Fatal("a stale confirm changed something")
	}
}

// A blank credential is accepted only when it is already stored.
func TestConnectorConfirm_BlankCredentialOnlyWhenStored(t *testing.T) {
	f := newConnectorFixture(t, nil)
	ctx := context.Background()
	pv, _ := f.pages.Preview(ctx, "agenda", f.choice(""))
	out, err := f.pages.Confirm(ctx, f.device, "agenda", f.choice(""), pv.Shown, nil)
	if err != nil || out.Installed || !strings.Contains(out.Refused, "CALENDAR_TOKEN") {
		t.Fatalf("a missing credential: %+v, %v", out, err)
	}
	if f.c.Registry.GetWorkflow("hermes--personal--agenda") != nil {
		t.Fatal("installed without its credential")
	}
}

// Review 99c9 F4: a connector demoted between the choose page and the
// confirm is refused, never installed as a plain recipe.
func TestConnectorConfirm_DemotedAfterPreviewIsRefused(t *testing.T) {
	f := newConnectorFixture(t, nil)
	ctx := context.Background()
	pv, err := f.pages.Preview(ctx, "agenda", f.choice(""))
	if err != nil || pv.Refused != "" {
		t.Fatalf("preview %+v, %v", pv, err)
	}
	path := filepath.Join(f.cfgDir, "configs", "agent-templates", "recipes", "agenda", "recipe.yaml")
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), "recipe_version: 1", "recipe_version: 2", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := agentadmin.NewRenderer(os.DirFS(filepath.Join(f.cfgDir, "configs", "agent-templates")))
	if err != nil {
		t.Fatal(err)
	}
	f.svc.renderer = r
	out, err := f.pages.Confirm(ctx, f.device, "agenda", f.choice(""), pv.Shown, map[string][]byte{"CALENDAR_TOKEN": []byte(credCanary)})
	if err != nil || out.Installed || out.Refused == "" {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	if f.c.Registry.GetWorkflow("hermes--personal--agenda") != nil {
		t.Fatal("a demoted connector was installed")
	}
}

// Review 99c9 F6: a blank credential field is accepted when the credential
// is already stored, and then nothing is entered for it.
func TestConnectorConfirm_BlankCredentialWhenStoredIsKept(t *testing.T) {
	f := newConnectorFixture(t, nil)
	ctx := context.Background()
	st, err := f.c.secretStoreForWrite()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "hermes", "CALENDAR_TOKEN", "secret", []byte(credCanary), f.device.ID); err != nil {
		t.Fatal(err)
	}
	pv, _ := f.pages.Preview(ctx, "agenda", f.choice(""))
	if len(pv.Credentials) != 1 || !pv.Credentials[0].Stored {
		t.Fatalf("preview credentials %+v", pv.Credentials)
	}
	out, err := f.pages.Confirm(ctx, f.device, "agenda", f.choice(""), pv.Shown, nil)
	if err != nil || !out.Installed || out.Failed != "" {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	if err := f.c.verifyAgentReach(ctx, "hermes--personal", "hermes--personal--agenda"); err != nil {
		t.Fatalf("not runnable: %v", err)
	}
}

// Item 5a: the device creates the connector's project in an existing
// namespace, named by the connector; nothing else may be named.
func TestConnectorPages_CreatesTheConnectorsProject(t *testing.T) {
	f := newConnectorFixture(t, nil)
	ctx := context.Background()
	if err := f.pages.CreateProject(ctx, f.device, "agenda", "hermes"); err != nil {
		t.Fatal(err)
	}
	if f.c.Registry.GetProject("hermes--agenda") == nil {
		t.Fatal("the project was not created")
	}
	if err := f.pages.CreateProject(ctx, f.device, "agenda", "nobody"); err == nil {
		t.Fatal("a project was created in a namespace that does not exist")
	}
	ns, err := f.pages.Namespaces(ctx)
	if err != nil || len(ns) != 1 || ns[0].Name != "hermes" || strings.Join(ns[0].Projects, ",") != "agenda,personal" {
		t.Fatalf("namespaces %+v, %v", ns, err)
	}
}

// §19.14.11 (operator decision 2026-10-13): a shipped recipe the operator
// vets in connector.yaml in the deployed tree is offered and installs in one
// confirm; the shipped tree alone offers none.
func TestConnectorPage_OffersAnOperatorVettedRecipe(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	pages := connectorPages{s: f.svc}
	if views, err := pages.List(ctx); err != nil || len(views) != 0 {
		t.Fatalf("the shipped tree offers %+v, %v", views, err)
	}
	cal := newFakeMCP(t, "Bearer "+credCanary, "calendar_list_events")
	dir := filepath.Join(f.cfgDir, "configs", "agent-templates", "recipes", "agenda")
	vetting := "title: \"Calendar (read)\"\nservers:\n  calendar: {url: \"" + cal.srv.URL + "\"}\nvetted: {by: \"operator\", at: \"2026-10-13\", recipe_version: 1}\n"
	if err := os.WriteFile(filepath.Join(dir, "connector.yaml"), []byte(vetting), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := agentadmin.NewRenderer(os.DirFS(filepath.Join(f.cfgDir, "configs", "agent-templates")))
	if err != nil {
		t.Fatal(err)
	}
	f.svc.renderer = r
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "personal", Purpose: "Personal automations"})
	views, err := pages.List(ctx)
	if err != nil || len(views) != 1 || views[0].Name != "agenda" {
		t.Fatalf("list %+v, %v", views, err)
	}
	cf := &connectorFixture{agentAdminFixture: f, cal: cal, pages: pages}
	if out := cf.confirm(cf.choice(""), map[string]string{"CALENDAR_TOKEN": credCanary}); !out.Installed || out.Failed != "" {
		t.Fatalf("outcome %+v", out)
	}
	if err := f.c.verifyAgentReach(ctx, "hermes--personal", "hermes--personal--agenda"); err != nil {
		t.Fatalf("not runnable: %v", err)
	}
}

// §19.14.6: the page lists the vetted connectors only: not a plain recipe,
// not a connector whose vetting is stale.
func TestConnectorPage_ListsVettedOnly(t *testing.T) {
	f := newConnectorFixture(t, nil)
	views, err := f.pages.List(context.Background())
	if err != nil || len(views) != 1 || views[0].Name != "agenda" {
		t.Fatalf("list %+v, %v", views, err)
	}
	v := views[0]
	if len(v.Reads) != 1 || strings.Join(v.Reads[0].Tools, ",") != "calendar_list_events" || v.Reads[0].Reads != "your calendar" || len(v.Returns) == 0 {
		t.Fatalf("view %+v", v)
	}
	g := newConnectorFixture(t, func(r string) string { return strings.Replace(r, "recipe_version: 1", "recipe_version: 2", 1) })
	if views, err := g.pages.List(context.Background()); err != nil || len(views) != 0 {
		t.Fatalf("a stale connector is listed: %+v, %v", views, err)
	}
}

// Review-20261013-f1f5 N1 (§19.14.8 Recovery): a confirm that stopped
// after the install is not resumable through the page, and the agent path
// completes it: request_credential, entry, then the tools approval
// afterCredentialStored files.
func TestConnectorConfirm_StepFailureFallsBackToAgentPath(t *testing.T) {
	f := newConnectorFixture(t, nil)
	ctx := context.Background()
	f.svc.connectorStep = func(step string) error {
		if step == "credential" {
			return errors.New("injected")
		}
		return nil
	}
	if out := f.confirm(f.choice(""), map[string]string{"CALENDAR_TOKEN": credCanary}); !out.Installed || out.Failed != "credential" {
		t.Fatalf("outcome %+v", out)
	}
	f.svc.connectorStep = nil
	if pv, err := f.pages.Preview(ctx, "agenda", f.choice("")); err != nil || pv.Refused == "" || pv.Shown != "" {
		t.Fatalf("a second confirm is offered: %+v, %v", pv, err)
	}
	if p := f.personal(); !hasLine(p.NextSteps, "CALENDAR_TOKEN") {
		t.Fatalf("next steps %v", p.NextSteps)
	}
	if res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "personal", Name: "CALENDAR_TOKEN", Purpose: "Read your calendar", Kind: "secret"}); res.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("request_credential: %+v", res)
	}
	slots := f.pendingSlots()
	if slots["CALENDAR_TOKEN"] == "" {
		t.Fatalf("slots %v", slots)
	}
	req, _ := f.c.repos.ApproverDevices.GetRequest(ctx, slots["CALENDAR_TOKEN"])
	if err := f.svc.enterCredential(ctx, f.device, *req, req.RenderedSHA256, []byte(credCanary)); err != nil {
		t.Fatal(err)
	}
	tools := pendingToolsRequest(t, f.agentAdminFixture)
	if err := f.decide(tools.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.c.verifyAgentReach(ctx, "hermes--personal", "hermes--personal--agenda"); err != nil {
		t.Fatalf("the agent path did not complete the install: %v", err)
	}
}
