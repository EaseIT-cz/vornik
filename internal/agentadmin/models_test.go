package agentadmin

import (
	"os"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Design §18.6 item 2 in detail (GREEN at review a125): a role may name a
// model from the operator's catalogue. Local or remote is computed from the
// live router, never declared; the approval binds the destination
// <sub-provider>@<endpoint host>; CLI-backed providers and unpriced remote
// entries are not offered.

// routes is a fake live router: model id → where it goes.
type routes map[string]ModelRoute

func (r routes) resolve(model string) (ModelRoute, bool) {
	rt, ok := r[model]
	return rt, ok
}

func prices(known ...string) ModelPricer {
	return func(model string) (float64, float64, bool) {
		for _, k := range known {
			if k == model {
				return 3, 15, true
			}
		}
		return 0, 0, false
	}
}

func testRoutes() routes {
	return routes{
		"qwen3:35b":           {SubProvider: "http", Endpoint: "http://127.0.0.1:11434/v1"},
		"lan-model":           {SubProvider: "http", Endpoint: "http://192.168.0.142:8000/v1"},
		"google/gemini-pro":   {SubProvider: "vertex", Endpoint: "https://aiplatform.googleapis.com/v1/projects/p/locations/global/endpoints/openapi"},
		"google/gemini-flash": {SubProvider: "vertex", Endpoint: "https://aiplatform.googleapis.com/v1/projects/p/locations/global/endpoints/openapi"},
		"claude-sonnet":       {SubProvider: "claude-cli", CLI: true},
		"unpriced-remote":     {SubProvider: "openrouter", Endpoint: "https://openrouter.ai/api/v1"},
	}
}

// §18.6 item 2 (Change 2; round 3 F1; round 2 F5): loopback and private
// endpoints are local, a cloud endpoint is remote, a CLI provider and an
// unresolvable id are dropped, an unpriced remote entry is dropped, and an
// unpriced local one is kept as "no charge recorded".
func TestBuildCatalogue_ClassifiesFromTheRoute(t *testing.T) {
	specs := []ModelSpec{
		{ID: "qwen3:35b", GoodFor: "everyday drafting"},
		{ID: "lan-model", GoodFor: "on the LAN"},
		{ID: "google/gemini-pro", GoodFor: "hard critique"},
		{ID: "claude-sonnet", GoodFor: "cli"},
		{ID: "nowhere", GoodFor: "unroutable"},
		{ID: "unpriced-remote", GoodFor: "no price"},
	}
	cat, findings := BuildCatalogue(specs, testRoutes().resolve, prices("google/gemini-pro", "claude-sonnet"))
	if m, ok := cat["qwen3:35b"]; !ok || !m.Dest.Local || m.PriceLabel() != "no charge recorded" {
		t.Errorf("loopback ollama: %+v ok=%v", m, ok)
	}
	if m, ok := cat["lan-model"]; !ok || !m.Dest.Local {
		t.Errorf("private address: %+v ok=%v", m, ok)
	}
	m, ok := cat["google/gemini-pro"]
	if !ok || m.Dest.Local || m.Dest.String() != "vertex@aiplatform.googleapis.com" || !strings.Contains(m.PriceLabel(), "$3") {
		t.Errorf("cloud: %+v ok=%v label %q", m, ok, m.PriceLabel())
	}
	for _, id := range []string{"claude-sonnet", "nowhere", "unpriced-remote"} {
		if _, ok := cat[id]; ok {
			t.Errorf("%s is offered; it must be dropped", id)
		}
		found := false
		for _, f := range findings {
			if f.ID == id && f.Why != "" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s dropped without a finding: %+v", id, findings)
		}
	}
}

// Round 2 F3: the destination is <sub-provider>@<endpoint host>, so an
// endpoint repointed under the same sub-provider name is a new destination.
func TestDestinationOf_BindsTheHost(t *testing.T) {
	a, why := DestinationOf(ModelRoute{SubProvider: "http", Endpoint: "https://api.one.example/v1"}, true)
	b, _ := DestinationOf(ModelRoute{SubProvider: "http", Endpoint: "https://api.two.example/v1"}, true)
	if why != "" || a.String() == b.String() || a.String() != "http@api.one.example" {
		t.Fatalf("%q vs %q (%s)", a, b, why)
	}
	if _, why := DestinationOf(ModelRoute{SubProvider: "codex-cli", CLI: true}, true); why == "" {
		t.Error("a CLI-backed provider was given a destination")
	}
	if _, why := DestinationOf(ModelRoute{}, false); why == "" {
		t.Error("an unresolvable model was given a destination")
	}
	// Review 20261003-2ed0 B1: an HTTP route with no endpoint is not
	// offered, never taken for local.
	if d, why := DestinationOf(ModelRoute{SubProvider: "bedrock", Endpoint: ""}, true); why == "" || d.Local {
		t.Errorf("an empty endpoint: %+v %q", d, why)
	}
}

func modelTree(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t, "hermes")
	cat, _ := BuildCatalogue([]ModelSpec{
		{ID: "qwen3:35b", GoodFor: "everyday drafting"},
		{ID: "google/gemini-pro", GoodFor: "hard critique"},
		{ID: "google/gemini-flash", GoodFor: "fast critique"},
	}, testRoutes().resolve, prices("google/gemini-pro", "google/gemini-flash"))
	tr.models = cat
	tr.project("finance")
	return tr
}

func roleWith(name, model string) RoleInput {
	return RoleInput{Name: name, Instructions: "Do the " + name + " job.", Tools: []string{"file_read"}, Model: model}
}

// Change 1 and §7.1's amended Refused row: a model outside the operator's
// catalogue is refused, and any model with an empty catalogue.
func TestDefineSwarm_ModelOutsideTheCatalogueIsRefused(t *testing.T) {
	tr := modelTree(t)
	c := tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{roleWith("critic", "gpt-5")}})
	if c.Class != Refused || !strings.Contains(c.Reason, "catalogue") {
		t.Fatalf("off-catalogue: %s %q", c.Class, c.Reason)
	}
	empty := newTree(t, "hermes")
	empty.project("finance")
	c = empty.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{roleWith("critic", "qwen3:35b")}})
	if c.Class != Refused || !strings.Contains(c.Reason, "agent_admin.models") {
		t.Fatalf("empty catalogue: %s %q", c.Class, c.Reason)
	}
}

// Change 4 (local): a role on a local model is an ordinary role change.
func TestDefineSwarm_LocalModelAppliesInert(t *testing.T) {
	tr := modelTree(t)
	c := tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{roleWith("drafter", "qwen3:35b")}})
	tr.mustClass(c, Inert)
	if len(c.Grant.Models) != 0 {
		t.Fatalf("a local model granted %+v", c.Grant.Models)
	}
	tr.apply(c)
	sw, err := registry.ParseSwarmMarkdown([]byte(tr.files["swarms/hermes--finance.md"]), "swarms/hermes--finance.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range sw.Roles {
		if r.Name == "drafter" && r.Model != "qwen3:35b" {
			t.Fatalf("the rendered role runs on %q", r.Model)
		}
		if r.ModelFallback != "" {
			t.Fatalf("role %s rendered with a modelFallback (review d94f F6)", r.Name)
		}
	}
}

// Change 4 (remote) with rounds 2 and 3: a remote model on a destination the
// namespace has not approved is widening; the sentence names the host, the
// plain view is the §18.7 remote-model phrase at level Medium; after the
// approval a second role on the same destination applies at once.
func TestDefineSwarm_RemoteModelIsWideningOncePerDestination(t *testing.T) {
	tr := modelTree(t)
	c := tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{roleWith("critic", "google/gemini-pro")}})
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Sentence, "sends what the critic role works on to vertex at aiplatform.googleapis.com") {
		t.Fatalf("sentence %q", c.Sentence)
	}
	if len(c.Grant.Models) != 1 || c.Grant.Models[0].Destination != "vertex@aiplatform.googleapis.com" || c.Grant.Models[0].Model != "google/gemini-pro" {
		t.Fatalf("grant %+v", c.Grant.Models)
	}
	if c.Plain == nil || c.Plain.Level != LevelMedium || !strings.Contains(c.Plain.Summary, "vertex at aiplatform.googleapis.com") {
		t.Fatalf("plain view %+v", c.Plain)
	}
	tr.apply(c)

	// A second role on the same destination, and a re-render of the
	// approved one, are inert.
	c = tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{
		roleWith("critic", "google/gemini-pro"), roleWith("editor", "google/gemini-flash")}})
	tr.mustClass(c, Inert)
}

// Round 2 tests added: a re-render of an approved remote-model role is inert.
func TestDefineSwarm_RerenderOfApprovedRemoteRoleIsInert(t *testing.T) {
	tr := modelTree(t)
	in := DefineSwarmInput{Slug: "finance", Roles: []RoleInput{roleWith("critic", "google/gemini-pro")}}
	tr.apply(tr.render(VerbDefineSwarm, in))
	tr.mustClass(tr.render(VerbDefineSwarm, in), Inert)
}

// Round 3 tests added: re-approval after a soft removal is a new widening
// (a removed row reads as absent).
func TestDefineSwarm_AfterSoftRemovalIsWideningAgain(t *testing.T) {
	tr := modelTree(t)
	in := DefineSwarmInput{Slug: "finance", Roles: []RoleInput{roleWith("critic", "google/gemini-pro")}}
	tr.apply(tr.render(VerbDefineSwarm, in))
	delete(tr.destinations, "vertex@aiplatform.googleapis.com") // the console set removed_at
	tr.mustClass(tr.render(VerbDefineSwarm, in), Widening)
}

// Change 8: read-back keeps the model, so a verb that re-renders the swarm
// writes the role back on the same model (the §18.11 lesson).
func TestReadBackThenRerender_KeepsTheModel(t *testing.T) {
	r, err := NewRenderer(os.DirFS("../../configs/agent-templates"))
	if err != nil {
		t.Fatal(err)
	}
	st, p := roleRoundTripState()
	p.Swarm.Roles[0].Model = "google/gemini-pro"
	loaded := renderAndParse(t, r, st, p)
	back := ProjectStateFrom(&registry.Project{ID: p.ID}, loaded)
	back.DisplayName, back.Swarm.ID = p.DisplayName, p.Swarm.ID
	if got := back.Swarm.Roles[0].Model; got != "google/gemini-pro" {
		t.Fatalf("read-back model %q", got)
	}
	again := renderAndParse(t, r, st, back)
	if again.Roles[0].Model != "google/gemini-pro" || again.Roles[1].Model != "" {
		t.Fatalf("after a re-render: %q, %q", again.Roles[0].Model, again.Roles[1].Model)
	}
}

// Round 2 F2 / round 3 F1: the run-time judgement, from the live route at
// that moment. A model off the catalogue, a CLI route, a remote destination
// not approved: refused, naming the destination. Local, or approved: runs.
func TestRunModelRefusal(t *testing.T) {
	specs := []ModelSpec{{ID: "qwen3:35b"}, {ID: "google/gemini-pro"}, {ID: "claude-sonnet"}}
	rts := testRoutes()
	approved := map[string]bool{}
	reasons := map[string]string{}
	judge := func(model string) string {
		_, r := RunModelRefusal(model, specs, rts.resolve, func(d string) bool { return approved[d] })
		reasons[model] = r.Reason
		if (r.Reason == "") != (r.Why == "") {
			t.Errorf("%s: a reason without a why or the reverse: %+v", model, r)
		}
		return r.Why
	}
	if why := judge("qwen3:35b"); why != "" {
		t.Errorf("local refused: %s", why)
	}
	if why := judge("gpt-5"); !strings.Contains(why, "catalogue") {
		t.Errorf("off-catalogue: %q", why)
	}
	if why := judge("google/gemini-pro"); !strings.Contains(why, "vertex@aiplatform.googleapis.com") {
		t.Errorf("unapproved remote: %q", why)
	}
	approved["vertex@aiplatform.googleapis.com"] = true
	if why := judge("google/gemini-pro"); why != "" {
		t.Errorf("approved remote refused: %s", why)
	}
	if why := judge("claude-sonnet"); why == "" {
		t.Error("a CLI route ran")
	}
	// Review 20261003-2ed0 B-extra: each refusal carries a low-cardinality
	// reason for vornik_agent_model_refusals_total.
	for model, want := range map[string]string{"gpt-5": RefusalOffCatalogue, "claude-sonnet": RefusalCLIRoute} {
		if reasons[model] != want {
			t.Errorf("%s: reason %q, want %q", model, reasons[model], want)
		}
	}
	// A route edit since load: the local model now goes to a remote host.
	rts["qwen3:35b"] = ModelRoute{SubProvider: "http", Endpoint: "https://api.cloud.example/v1"}
	if why := judge("qwen3:35b"); !strings.Contains(why, "http@api.cloud.example") || reasons["qwen3:35b"] != RefusalUnapproved {
		t.Errorf("repointed local model: %q (%s)", why, reasons["qwen3:35b"])
	}
}

// Design §18.14 finding 1, round 2 F6 (GREEN at review be5a): each role of
// list_my_setup says where it runs, computed with the run-time judgement
// (RunModelRefusal) so the setup and the executor cannot disagree: default
// (no model), local, approved:<destination>, needs_approval:<destination>.
// A model the run-time check refuses for another reason is
// unavailable:<reason> (as built, 2026-10-03).
func TestRoleRunsOn(t *testing.T) {
	specs := []ModelSpec{{ID: "qwen3:35b"}, {ID: "google/gemini-pro"}, {ID: "claude-sonnet"}}
	rts := testRoutes()
	approved := map[string]bool{}
	on := func(model string) string {
		return RoleRunsOn(model, specs, rts.resolve, func(d string) bool { return approved[d] })
	}
	cases := []struct{ model, want string }{
		{"", runsOnDefault},
		{"qwen3:35b", runsOnLocal},
		{"google/gemini-pro", "needs_approval:vertex@aiplatform.googleapis.com"},
		{"gpt-5", "unavailable:" + RefusalOffCatalogue},
		{"claude-sonnet", "unavailable:" + RefusalCLIRoute},
	}
	for _, c := range cases {
		if got := on(c.model); got != c.want {
			t.Errorf("RoleRunsOn(%q) = %q, want %q", c.model, got, c.want)
		}
	}
	approved["vertex@aiplatform.googleapis.com"] = true
	if got := on("google/gemini-pro"); got != "approved:vertex@aiplatform.googleapis.com" {
		t.Errorf("approved remote: %q", got)
	}
}
