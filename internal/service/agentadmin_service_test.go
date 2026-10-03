package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// agentAdminFixture boots a real container with the agent templates
// deployed, a paired approver device and an agent admin key for "hermes".
type agentAdminFixture struct {
	t      *testing.T
	c      *Container
	svc    *agentAdminService
	key    *persistence.APIKey
	device *approverdevice.Device
	cfgDir string
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() { // the recipes catalogue (design §19)
			copyDir(t, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newAgentAdminFixture(t *testing.T) *agentAdminFixture {
	t.Helper()
	return newAgentAdminFixtureWith(t, nil)
}

// newAgentAdminFixtureWith lets a test adjust the config before boot.
func newAgentAdminFixtureWith(t *testing.T, adjust func(*config.Config)) *agentAdminFixture {
	t.Helper()
	cfg := newComposerWiringTestConfig(t)
	cfg.Node.Profile = "" // all: the apply engine's busy check needs the task store
	if adjust != nil {
		adjust(cfg)
	}
	path := isolatedConfigPath(t)
	copyDir(t, "../../configs/agent-templates", filepath.Join(filepath.Dir(path), "configs", "agent-templates"))
	// An apply reloads, and a reload re-reads config.yaml: write the config
	// this container boots with, as a deployment has it on disk.
	cfg.Server.Address = "127.0.0.1:0"
	rawCfg, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rawCfg, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewContainer(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	svc := c.agentAdmin()
	if svc == nil {
		t.Fatal("the agent admin service is not wired")
	}
	leaseCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := c.StartConfigWriterLease(leaseCtx); err != nil { // as the daemon does at boot
		t.Fatal(err)
	}
	ctx := context.Background()
	devices := c.approverDeviceService()
	code, _, _ := devices.StartPairing(ctx, "Phone")
	red, err := devices.Redeem(ctx, code, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return &agentAdminFixture{t: t, c: c, svc: svc, device: red.Device, cfgDir: filepath.Dir(path),
		key: &persistence.APIKey{ID: "akey_hermes", ClientKind: "hermes", AgentAdmin: true, AgentNamespace: "hermes"}}
}

func (f *agentAdminFixture) do(verb string, in any) agentadmin.Result {
	f.t.Helper()
	raw, _ := json.Marshal(in)
	res, err := f.svc.Do(context.Background(), f.key, verb, raw)
	if err != nil {
		f.t.Fatalf("%s: %v", verb, err)
	}
	return res
}

// approve decides the request behind an awaiting_approval result as the
// fixture's device.
func (f *agentAdminFixture) approve(res agentadmin.Result) {
	f.t.Helper()
	if res.Effect != agentadmin.EffectAwaiting {
		f.t.Fatalf("not awaiting approval: %+v", res)
	}
	id := res.ApprovalURL[strings.LastIndex(res.ApprovalURL, "/")+1:]
	req, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.c.approverDeviceService().Decide(context.Background(), f.device, id, req.RenderedSHA256, true); err != nil {
		f.t.Fatalf("decide: %v", err)
	}
}

// The inert path end to end: rendered, filed, approved by the system actor,
// applied, reloaded, and verified in the live registry.
func TestAgentAdmin_CreateProjectAppliesAndLoads(t *testing.T) {
	f := newAgentAdminFixture(t)
	res := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	if res.Effect != agentadmin.EffectApplied {
		t.Fatalf("create_project: %+v", res)
	}
	if f.c.Registry.GetProject("hermes--finance") == nil || f.c.Registry.GetSwarm("hermes--finance") == nil ||
		f.c.Registry.GetWorkflow("hermes--finance--start") == nil {
		t.Fatal("the project did not load")
	}
	p, err := f.c.repos.Proposals.GetByID(context.Background(), res.ChangeID)
	if err != nil || p.Status != persistence.ProposalStatusApplied || p.ProposedBy != "agent:hermes" || p.AppliedBy != AgentAdminInertActor {
		t.Fatalf("ledger row: %+v, %v", p, err)
	}
	again := f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "again"})
	if again.Effect != agentadmin.EffectRefused || !strings.Contains(again.Reason, "already exists") {
		t.Fatalf("a duplicate was not refused: %+v", again)
	}
}

// The widening path end to end: filed DRAFT with a request, pushed; the
// device approves; the effect approves the proposal as the device, applies
// it, and records the grant.
func TestAgentAdmin_WideningNeedsTheDevice(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	in := agentadmin.DefineWorkflowInput{Project: "finance", Slug: "spend",
		Steps:  []agentadmin.StepInput{{Name: "sum", Role: "worker", Instructions: "Sum the month."}},
		Egress: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"total":{"type":"number"}}}`)}
	res := f.do(agentadmin.VerbDefineWorkflow, in)
	if res.Effect != agentadmin.EffectAwaiting || !strings.Contains(res.ApprovalURL, "/ui/approve/apr_") {
		t.Fatalf("define_workflow: %+v", res)
	}
	if f.c.Registry.GetWorkflow("hermes--finance--spend") != nil {
		t.Fatal("a widening change applied before approval")
	}
	// The pending change holds its entities: a second change to them is refused.
	if second := f.do(agentadmin.VerbDefineWorkflow, in); second.Effect != agentadmin.EffectRefused || !strings.Contains(second.Reason, "waiting for approval") {
		t.Fatalf("a second change to a pending entity: %+v", second)
	}
	f.approve(res)
	if f.c.Registry.GetWorkflow("hermes--finance--spend") == nil {
		t.Fatal("the approved workflow did not load")
	}
	p, _ := f.c.repos.Proposals.GetByID(context.Background(), res.ChangeID)
	if p.Status != persistence.ProposalStatusApplied || p.Approver != "device:"+f.device.ID {
		t.Fatalf("ledger: status %s approver %q", p.Status, p.Approver)
	}
	reach, err := f.c.repos.AgentGrants.GetWorkflowReach(context.Background(), "hermes--finance--spend")
	if err != nil || reach.ReachHash == "" || reach.ApprovedByDevice != f.device.ID {
		t.Fatalf("the grant was not recorded: %+v, %v", reach, err)
	}
	// Now identical: inert.
	if again := f.do(agentadmin.VerbDefineWorkflow, in); again.Effect != agentadmin.EffectApplied {
		t.Fatalf("an unchanged workflow is not inert after approval: %+v", again)
	}
}

// A rejected request rejects its proposal; nothing applies.
func TestAgentAdmin_RejectedChangeNeverApplies(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	res := f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "finance", MonthlyUSD: 5})
	id := res.ApprovalURL[strings.LastIndex(res.ApprovalURL, "/")+1:]
	req, _ := f.c.repos.ApproverDevices.GetRequest(context.Background(), id)
	if err := f.c.approverDeviceService().Decide(context.Background(), f.device, id, req.RenderedSHA256, false); err != nil {
		t.Fatal(err)
	}
	p, _ := f.c.repos.Proposals.GetByID(context.Background(), res.ChangeID)
	if p.Status != persistence.ProposalStatusRejected {
		t.Fatalf("the proposal behind a rejected request is %s", p.Status)
	}
	if got := f.c.Registry.GetProject("hermes--finance").Budget.MonthlyHardUSD; got != 2 {
		t.Fatalf("budget = %v after a rejection", got)
	}
}

// Review focus 4: a hand edit between filing and approval ends the request
// failed with the reason; the operator's edit survives.
func TestAgentAdmin_StaleApprovalFailsWithReason(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	res := f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "finance", MonthlyUSD: 5})
	projectFile := filepath.Join(f.cfgDir, "configs", "projects", "hermes--finance.yaml")
	b, _ := os.ReadFile(projectFile)
	edited := strings.Replace(string(b), "maxConcurrentTasks: 1", "maxConcurrentTasks: 2", 1)
	if err := os.WriteFile(projectFile, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	id := res.ApprovalURL[strings.LastIndex(res.ApprovalURL, "/")+1:]
	req, _ := f.c.repos.ApproverDevices.GetRequest(context.Background(), id)
	err := f.c.approverDeviceService().Decide(context.Background(), f.device, id, req.RenderedSHA256, true)
	if err == nil {
		t.Fatal("a stale change applied")
	}
	got, _ := f.c.repos.ApproverDevices.GetRequest(context.Background(), id)
	if got.AppliedAt == nil || got.ApplyError == "" {
		t.Fatalf("the failure was not recorded: %+v", got)
	}
	if now, _ := os.ReadFile(projectFile); string(now) != edited {
		t.Fatal("the operator's edit was overwritten")
	}
}

// Plan P3.1: the grant's home project is created through the inert path,
// once, and the agent cannot remove it.
func TestAgentAdmin_EnsureHomeIsIdempotentAndPermanent(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		id, err := f.svc.EnsureHome(ctx, "hermes", "hermes")
		if err != nil || id != "hermes--home" {
			t.Fatalf("EnsureHome #%d = %q, %v", i, id, err)
		}
	}
	if f.c.Registry.GetProject("hermes--home") == nil {
		t.Fatal("the home project did not load")
	}
	f.key.ProjectID = "hermes--home"
	if res := f.do(agentadmin.VerbRemove, agentadmin.RemoveInput{Kind: "project", ID: "home"}); res.Effect != agentadmin.EffectRefused {
		t.Fatalf("the home project was removable: %+v", res)
	}
}

// Review focus 5: two namespaces side by side. Neither sees, names or
// reaches the other's objects through any verb.
func TestAgentAdmin_NamespacesAreIsolated(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Hermes money"})
	codex := &persistence.APIKey{ID: "akey_codex", ClientKind: "codex", AgentAdmin: true, AgentNamespace: "codex"}

	setup, err := f.svc.ListSetup(ctx, codex)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(setup)
	if strings.Contains(string(raw), "hermes") || strings.Contains(string(raw), "Hermes money") {
		t.Fatalf("codex's setup names hermes's objects: %s", raw)
	}
	for verb, in := range map[string]any{
		agentadmin.VerbDefineWorkflow: agentadmin.DefineWorkflowInput{Project: "finance", Slug: "w",
			Steps:  []agentadmin.StepInput{{Name: "s", Role: "worker", Instructions: "x"}},
			Egress: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"a":{"type":"number"}}}`)},
		agentadmin.VerbSetBudget: agentadmin.SetBudgetInput{Project: "finance", MonthlyUSD: 1},
		agentadmin.VerbRemove:    agentadmin.RemoveInput{Kind: "project", ID: "finance"},
	} {
		b, _ := json.Marshal(in)
		res, err := f.svc.Do(ctx, codex, verb, b)
		if err != nil || res.Effect != agentadmin.EffectRefused {
			t.Fatalf("codex %s on hermes's project: %+v, %v", verb, res, err)
		}
		if strings.Contains(res.Reason, "hermes") {
			t.Fatalf("the refusal names the other namespace: %q", res.Reason)
		}
	}
	if f.c.Registry.GetProject("hermes--finance") == nil {
		t.Fatal("hermes's project was touched")
	}
}

// Plan P3.9 (§7.6): an agent workflow runs only with the reach a device
// approved, recomputed from the LIVE registry. Control: verifyAgentReach.
// Without it, a hand edit re-points an approved workflow at a new reach and
// it runs.
func TestAgentAdmin_ReachVerifiedAgainstTheLiveRegistry(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	in := agentadmin.DefineWorkflowInput{Project: "finance", Slug: "spend",
		Steps:  []agentadmin.StepInput{{Name: "sum", Role: "worker", Instructions: "Sum the month."}},
		Egress: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"total":{"type":"number"}}}`)}
	f.approve(f.do(agentadmin.VerbDefineWorkflow, in))

	if err := f.c.verifyAgentReach(ctx, "hermes--finance", "hermes--finance--spend"); err != nil {
		t.Fatalf("the approved workflow was refused: %v", err)
	}
	if err := f.c.verifyAgentReach(ctx, "hermes--finance", "hermes--finance--start"); err == nil {
		t.Fatal("the never-approved placeholder workflow was allowed to run")
	}
	if err := f.c.verifyAgentReach(ctx, "hermes--home", "hermes--finance--spend"); err == nil {
		t.Fatal("a workflow ran in a project that does not own it")
	}
	if err := f.c.verifyAgentReach(ctx, "assistant", "plain-workflow"); err != nil {
		t.Fatalf("an operator workflow was checked: %v", err)
	}

	// A hand edit gives the worker role a server's tool (and the project the
	// server). The files still load, but the reach no longer matches.
	cfgs := filepath.Join(f.cfgDir, "configs")
	swarm := filepath.Join(cfgs, "swarms", "hermes--finance.md")
	b, _ := os.ReadFile(swarm)
	edited := strings.Replace(string(b), `          - "file_write"`, "          - \"file_write\"\n          - \"mcp__mail__read\"", 1)
	if edited == string(b) {
		t.Fatal("the edit did not apply; the template changed shape")
	}
	if err := os.WriteFile(swarm, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.c.ConfigReloader.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := f.c.verifyAgentReach(ctx, "hermes--finance", "hermes--finance--spend"); err == nil || !strings.Contains(err.Error(), "changed what it returns or can reach") {
		t.Fatalf("a re-pointed workflow was allowed to run: %v", err)
	}
}

// Review 20261002-a048 F3: without the approval tables (a store that lacks
// them), the reach verifier must still be installed and refuse agent
// workflows; an unset verifier would admit them unchecked. Control:
// wireReachVerifier. Operator workflows are unaffected.
func TestWireReachVerifier_FailsClosedWithoutGrants(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}, Registry: registry.New()}
	opts := c.wireReachVerifier(nil)
	if len(opts) != 1 {
		t.Fatalf("the creator was given %d reach options with no grant store, want 1", len(opts))
	}
	ctx := context.Background()
	agentP, agentWF := &registry.Project{ID: "hermes--fin", Broker: true}, &registry.Workflow{ID: "hermes--fin--start"}
	if err := c.verifyAgentReachOf(ctx, agentP, nil, agentWF); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("an agent workflow with no approval tables: %v", err)
	}
	if err := c.verifyAgentReachOf(ctx, &registry.Project{ID: "assistant"}, nil, &registry.Workflow{ID: "plain"}); err != nil {
		t.Fatalf("an operator workflow was refused: %v", err)
	}
	if err := c.verifyAgentReachOf(ctx, &registry.Project{ID: "assistant"}, nil, agentWF); err == nil {
		t.Fatal("an agent workflow ran in an operator project")
	}
}
