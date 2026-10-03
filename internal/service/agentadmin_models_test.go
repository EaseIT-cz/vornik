package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Agent-administered design §18.6 item 2 in detail (GREEN at review a125):
// the service end of a role's model. The catalogue is the operator's; where
// a model goes is read from the live router; a remote destination is
// approved once per namespace on the device, re-resolved at apply, and
// re-checked before every attempt.

const (
	vertexEndpoint = "https://aiplatform.googleapis.com/v1/projects/p/locations/global/endpoints/openapi"
	vertexDest     = "vertex@aiplatform.googleapis.com"
)

// modelFixture boots the agent admin fixture with a two-model catalogue: a
// local one and a remote one, with a fake live router and pricing.
func modelFixture(t *testing.T) (*agentAdminFixture, map[string]agentadmin.ModelRoute) {
	t.Helper()
	f := newAgentAdminFixtureWith(t, func(cfg *config.Config) {
		cfg.AgentAdmin.Models = []config.AgentModelConfig{
			{ID: "qwen3:35b", GoodFor: "everyday drafting"},
			{ID: "google/gemini-pro", GoodFor: "hard critique"},
			{ID: "google/gemini-flash", GoodFor: "fast critique"},
		}
	})
	routes := map[string]agentadmin.ModelRoute{
		"qwen3:35b":           {SubProvider: "http", Endpoint: "http://127.0.0.1:11434/v1"},
		"google/gemini-pro":   {SubProvider: "vertex", Endpoint: vertexEndpoint},
		"google/gemini-flash": {SubProvider: "vertex", Endpoint: vertexEndpoint},
	}
	f.c.modelRoutes = func(model string) (agentadmin.ModelRoute, bool) {
		r, ok := routes[model]
		return r, ok
	}
	f.c.modelPrices = func(model string) (float64, float64, bool) {
		if strings.HasPrefix(model, "google/") {
			return 1.25, 10, true
		}
		return 0, 0, false
	}
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Money"})
	return f, routes
}

func swarmRoles(roles ...agentadmin.RoleInput) agentadmin.DefineSwarmInput {
	return agentadmin.DefineSwarmInput{Slug: "finance", Roles: roles}
}

func modelRole(name, model string) agentadmin.RoleInput {
	return agentadmin.RoleInput{Name: name, Instructions: "Do the " + name + " job.", Tools: []string{"file_read"}, Model: model}
}

// Change 4 end to end: a local model applies; a remote one waits for the
// device, whose approval records the destination; then a second role on it
// applies at once, and the live swarm runs each role on its model.
func TestAgentAdminModels_RemoteDestinationApprovedOnce(t *testing.T) {
	f, _ := modelFixture(t)
	ctx := context.Background()
	if res := f.do(agentadmin.VerbDefineSwarm, swarmRoles(modelRole("drafter", "qwen3:35b"))); res.Effect != agentadmin.EffectApplied {
		t.Fatalf("local model: %+v", res)
	}
	res := f.do(agentadmin.VerbDefineSwarm, swarmRoles(modelRole("drafter", "qwen3:35b"), modelRole("critic", "google/gemini-pro")))
	if res.Effect != agentadmin.EffectAwaiting || !strings.Contains(res.Sentence, "vertex at aiplatform.googleapis.com") {
		t.Fatalf("remote model: %+v", res)
	}
	if _, err := f.c.repos.AgentGrants.GetModelDestination(ctx, "hermes", vertexDest); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a destination was recorded before approval: %v", err)
	}
	f.approve(res)
	row, err := f.c.repos.AgentGrants.GetModelDestination(ctx, "hermes", vertexDest)
	if err != nil || row.ApprovedByDevice != f.device.ID || row.RemovedAt != nil {
		t.Fatalf("approval row: %+v, %v", row, err)
	}
	if res := f.do(agentadmin.VerbDefineSwarm, swarmRoles(modelRole("drafter", "qwen3:35b"), modelRole("critic", "google/gemini-pro"),
		modelRole("editor", "google/gemini-flash"))); res.Effect != agentadmin.EffectApplied {
		t.Fatalf("a second role on an approved destination: %+v", res)
	}
	got := map[string]string{}
	for _, r := range f.c.Registry.GetSwarm("hermes--finance").Roles {
		got[r.Name] = r.Model
	}
	if got["drafter"] != "qwen3:35b" || got["critic"] != "google/gemini-pro" || got["editor"] != "google/gemini-flash" || got["worker"] != "" {
		t.Fatalf("live roles: %v", got)
	}
}

// Round 3 F2 (tests added: a route edit between filing and approval makes
// the apply record nothing and fail the request).
func TestAgentAdminModels_RouteChangedBeforeApprovalRecordsNothing(t *testing.T) {
	f, routes := modelFixture(t)
	ctx := context.Background()
	res := f.do(agentadmin.VerbDefineSwarm, swarmRoles(modelRole("critic", "google/gemini-pro")))
	if res.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("remote model: %+v", res)
	}
	routes["google/gemini-pro"] = agentadmin.ModelRoute{SubProvider: "vertex", Endpoint: "https://europe-west4-aiplatform.googleapis.com/v1"}
	id := res.ApprovalURL[strings.LastIndex(res.ApprovalURL, "/")+1:]
	req, _ := f.c.repos.ApproverDevices.GetRequest(ctx, id)
	if err := f.c.approverDeviceService().Decide(ctx, f.device, id, req.RenderedSHA256, true); err == nil {
		t.Fatal("an approval whose route moved applied")
	}
	got, _ := f.c.repos.ApproverDevices.GetRequest(ctx, id)
	if got.ApplyError == "" || !strings.Contains(got.ApplyError, "the route changed after you were asked") {
		t.Fatalf("apply error %q", got.ApplyError)
	}
	for _, d := range []string{vertexDest, "vertex@europe-west4-aiplatform.googleapis.com"} {
		if _, err := f.c.repos.AgentGrants.GetModelDestination(ctx, "hermes", d); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("%s was recorded: %v", d, err)
		}
	}
	p, _ := f.c.repos.Proposals.GetByID(ctx, res.ChangeID)
	if p.Status == persistence.ProposalStatusApplied {
		t.Fatal("the change applied")
	}
	for _, r := range f.c.Registry.GetSwarm("hermes--finance").Roles {
		if r.Name == "critic" {
			t.Fatal("the critic role is live")
		}
	}
}

// Change 3 with round 3 F4: describe_installation lists the catalogue with
// what each model is good for, where it goes and its price; and no longer
// says a model cannot be changed.
func TestAgentAdminModels_DescribeListsTheCatalogue(t *testing.T) {
	f, _ := modelFixture(t)
	caps, err := f.svc.Describe(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]DescribedModel{}
	for _, m := range caps.Models {
		byID[m.ID] = m
	}
	local, remote := byID["qwen3:35b"], byID["google/gemini-pro"]
	if local.Where != "local" || local.Price != "no charge recorded" || local.GoodFor != "everyday drafting" {
		t.Errorf("local: %+v", local)
	}
	if remote.Where != "remote: vertex at aiplatform.googleapis.com" || !strings.Contains(remote.Price, "$1.25 in") || !strings.Contains(remote.Price, "$10 out") {
		t.Errorf("remote: %+v", remote)
	}
	for _, s := range caps.MayNot {
		if s == "change a model" {
			t.Error("may_not still says a model cannot be changed")
		}
	}
	// Review 20261003-2ed0 B5: say where a role with no model goes.
	if !strings.Contains(caps.ModelsNote, "no model runs on the operator's default model") || !strings.Contains(caps.ModelsNote, "may be remote") {
		t.Errorf("models_note: %q", caps.ModelsNote)
	}
	joined := strings.Join(caps.NeedsApproval, "; ")
	if !strings.Contains(joined, "remote model") {
		t.Errorf("needs_approval does not name a remote model: %s", joined)
	}
}

// Round 2 F2 and the run-time tests: the executor's model check, on the
// live route at that moment. A local model runs; a remote one without an
// approval, with a removed approval, after leaving the catalogue, or now
// served by a CLI is refused REACH_NOT_APPROVED-shaped; an approved one runs.
func TestVerifyAgentModel(t *testing.T) {
	f, routes := modelFixture(t)
	ctx := context.Background()
	check := func(model string) error { return f.c.verifyAgentModel(ctx, "hermes--finance", "critic", model) }
	if err := check("qwen3:35b"); err != nil {
		t.Fatalf("local: %v", err)
	}
	if err := check("google/gemini-pro"); err == nil || !strings.Contains(err.Error(), vertexDest) {
		t.Fatalf("unapproved remote: %v", err)
	}
	now := time.Now().UTC()
	if err := f.c.repos.AgentGrants.UpsertModelDestination(ctx, persistence.AgentModelDestinationApproval{Namespace: "hermes", Destination: vertexDest, ApprovedByDevice: f.device.ID, ApprovedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := check("google/gemini-pro"); err != nil {
		t.Fatalf("approved remote: %v", err)
	}
	// A repointed endpoint under the approved sub-provider is a new
	// destination (round 2 tests added).
	routes["google/gemini-pro"] = agentadmin.ModelRoute{SubProvider: "vertex", Endpoint: "https://evil.example/v1"}
	if err := check("google/gemini-pro"); err == nil || !strings.Contains(err.Error(), "vertex@evil.example") {
		t.Fatalf("repointed endpoint: %v", err)
	}
	routes["google/gemini-pro"] = agentadmin.ModelRoute{SubProvider: "vertex", Endpoint: vertexEndpoint}
	if _, err := f.c.repos.AgentGrants.MarkModelDestinationRemoved(ctx, "hermes", vertexDest, now); err != nil {
		t.Fatal(err)
	}
	if err := check("google/gemini-pro"); err == nil {
		t.Fatal("a removed approval still runs")
	}
	if err := check("gpt-5"); err == nil || !strings.Contains(err.Error(), "catalogue") {
		t.Fatalf("off-catalogue: %v", err)
	}
	routes["qwen3:35b"] = agentadmin.ModelRoute{SubProvider: "claude-cli", CLI: true}
	if err := check("qwen3:35b"); err == nil || !strings.Contains(err.Error(), "command-line") {
		t.Fatalf("now on a CLI: %v", err)
	}
	// An operator project's roles are not judged here (round 3 F6).
	if err := f.c.verifyAgentModel(ctx, "ops", "critic", "gpt-5"); err != nil {
		t.Fatalf("operator project: %v", err)
	}
}

// Review 20261003-2ed0 B-extra: the executor's own check, as the daemon
// wires it, refuses an agent step whose model the router now sends to a CLI
// provider, REACH_NOT_APPROVED, and counts it by reason.
func TestExecutorModelCheck_CLIRouteRefusedAndCounted(t *testing.T) {
	f, routes := modelFixture(t)
	if f.c.Executor == nil {
		t.Fatal("the container has no executor")
	}
	routes["qwen3:35b"] = agentadmin.ModelRoute{SubProvider: "claude-cli", CLI: true}
	before := testutil.ToFloat64(agentModelRefusals.WithLabelValues(agentadmin.RefusalCLIRoute))
	role := &registry.SwarmRole{Name: "drafter", Model: "qwen3:35b"}
	err := f.c.Executor.CheckAgentModel(context.Background(), "hermes--finance", role, "qwen3:35b")
	var classed interface{ FailureClass() string }
	if err == nil || !errors.As(err, &classed) || classed.FailureClass() != persistence.TaskFailureClassReachNotApproved {
		t.Fatalf("a CLI-routed agent model: %v", err)
	}
	if got := testutil.ToFloat64(agentModelRefusals.WithLabelValues(agentadmin.RefusalCLIRoute)) - before; got != 1 {
		t.Fatalf("vornik_agent_model_refusals_total{reason=cli_route} moved by %v", got)
	}
}

// Review 20261003-a525 A5: a removed approval is absent both from the state
// define_swarm renders against and from the run-time check.
func TestModelDestinations_RemovedRowsExcludedEverywhere(t *testing.T) {
	f, _ := modelFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := f.c.repos.AgentGrants.UpsertModelDestination(ctx, persistence.AgentModelDestinationApproval{Namespace: "hermes", Destination: vertexDest, ApprovedByDevice: f.device.ID, ApprovedAt: now}); err != nil {
		t.Fatal(err)
	}
	st, err := f.svc.loadState(ctx, "hermes", "hermes--home")
	if err != nil || !st.ApprovedDestinations[vertexDest] {
		t.Fatalf("a live approval is missing from the state: %v %v", st.ApprovedDestinations, err)
	}
	if _, err := f.c.repos.AgentGrants.MarkModelDestinationRemoved(ctx, "hermes", vertexDest, now); err != nil {
		t.Fatal(err)
	}
	st, err = f.svc.loadState(ctx, "hermes", "hermes--home")
	if err != nil || st.ApprovedDestinations[vertexDest] {
		t.Fatalf("a removed approval is in the state: %v %v", st.ApprovedDestinations, err)
	}
	if err := f.c.verifyAgentModel(ctx, "hermes--finance", "critic", "google/gemini-pro"); err == nil {
		t.Fatal("a removed approval still runs")
	}
}

// Round 2 tests added: a catalogue id that resolves through router.default
// is classified by the default's endpoint, through the live router's own
// decision (Resolves), never a copy of the route table.
func TestResolveModelRoute_ThroughTheLiveRouter(t *testing.T) {
	cfg := config.ChatConfig{Provider: "router"}
	cfg.Router.Default = "http"
	cfg.Router.HTTP.Endpoint = "http://127.0.0.1:11434/v1"
	cfg.Router.Vertex.ProjectID, cfg.Router.Vertex.Location = "p", "global"
	cfg.Router.ClaudeCLI.Enabled = true
	stub := chat.NewClient("http://127.0.0.1:1/v1", "k", "m")
	router, err := chat.NewRouter(stub, []chat.Route{
		{Prefix: "google/", Provider: stub, Name: "vertex"},
		{Prefix: "claude-", Provider: stub, Name: "claude-cli"},
	}, chat.WithRouterFallbackName("http"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"qwen3:35b":         "http@127.0.0.1",
		"google/gemini-pro": "vertex@aiplatform.googleapis.com",
	}
	for model, want := range cases {
		rt, ok := resolveModelRoute(cfg, router, model)
		d, why := agentadmin.DestinationOf(rt, ok)
		if why != "" || d.String() != want {
			t.Errorf("%s: %q (%s), want %q", model, d.String(), why, want)
		}
	}
	if rt, ok := resolveModelRoute(cfg, router, "claude-sonnet"); !ok || !rt.CLI {
		t.Errorf("claude-cli: %+v %v", rt, ok)
	}
	// Edit router.default: the same id is now classified by the new default.
	cfg.Router.Default = "openrouter"
	router2, _ := chat.NewRouter(stub, nil, chat.WithRouterFallbackName("openrouter"))
	rt, ok := resolveModelRoute(cfg, router2, "qwen3:35b")
	if d, _ := agentadmin.DestinationOf(rt, ok); d.String() != "openrouter@openrouter.ai" || d.Local {
		t.Errorf("after a default edit: %+v", d)
	}
	// No router: the single configured provider decides.
	single := config.ChatConfig{Provider: "http", Endpoint: "https://api.example.com/v1"}
	if rt, ok := resolveModelRoute(single, nil, "anything"); !ok || rt.Endpoint != "https://api.example.com/v1" {
		t.Errorf("single provider: %+v %v", rt, ok)
	}
	if rt, ok := resolveModelRoute(config.ChatConfig{Provider: "claude-cli"}, nil, "anything"); !ok || !rt.CLI {
		t.Errorf("single CLI provider: %+v %v", rt, ok)
	}
}

// Review 20261003-2ed0 B1: bedrock with no region has no endpoint; it is
// not offered (the catalogue drops it with a finding), not an empty host.
func TestRouterSubEndpoint_BedrockWithoutRegionIsNotOffered(t *testing.T) {
	if ep, _, ok := routerSubEndpoint(config.ChatConfig{}, "bedrock"); ok || ep != "" {
		t.Fatalf("bedrock without a region: %q ok=%v", ep, ok)
	}
	cfg := config.ChatConfig{Provider: "router"}
	cfg.Router.Bedrock.Region = "eu-central-1"
	if ep, _, ok := routerSubEndpoint(cfg, "bedrock"); !ok || ep != "https://bedrock-runtime.eu-central-1.amazonaws.com" {
		t.Fatalf("bedrock with a region: %q ok=%v", ep, ok)
	}
	stub := chat.NewClient("http://127.0.0.1:1/v1", "k", "m")
	router, _ := chat.NewRouter(stub, nil, chat.WithRouterFallbackName("bedrock"))
	cat, findings := agentadmin.BuildCatalogue([]agentadmin.ModelSpec{{ID: "anthropic.claude"}},
		func(m string) (agentadmin.ModelRoute, bool) {
			return resolveModelRoute(config.ChatConfig{Provider: "router"}, router, m)
		},
		func(string) (float64, float64, bool) { return 1, 1, true })
	if len(cat) != 0 || len(findings) != 1 {
		t.Fatalf("catalogue %v findings %v", cat, findings)
	}
}
