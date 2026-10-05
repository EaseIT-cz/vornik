package agentadmin

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func fioAPI() AddAPIInput {
	return AddAPIInput{Project: "finance", Name: "fio", BaseURL: "https://api.fio.example/v1",
		Auth: APIAuthInput{Credential: "FIO", Prefix: "Bearer "}, Methods: []string{"GET"}}
}

// Design §8.3, plan P4.5: add_api renders a per-project REST provider, is
// widening, grants its read methods, and names its credential; write
// methods are proposable only (writes must agree with methods). Control:
// addAPI.
func TestAddAPI(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	c := tr.render(VerbAddAPI, fioAPI())
	tr.mustClass(c, Widening)
	g := c.Grant.Integrations[0]
	if g.Kind != "api" || g.Name != "fio" || strings.Join(g.Read, ",") != "GET" || len(g.Write) != 0 || g.ReadPending {
		t.Fatalf("grant %+v", g)
	}
	for _, want := range []string{"fio", "api.fio.example", "GET", "FIO"} {
		if !strings.Contains(c.Sentence, want) {
			t.Errorf("sentence lacks %q: %s", want, c.Sentence)
		}
	}
	if !strings.Contains(c.Ops[0].Content, `value_from: "secret://hermes/FIO"`) || !strings.Contains(c.Ops[0].Content, `"hermes/FIO"`) {
		t.Fatalf("rendered:\n%s", c.Ops[0].Content)
	}
	tr.apply(c)
	apis := tr.state().Projects["hermes--finance"].APIs
	if len(apis) != 1 || apis[0].AuthRef != "secret://hermes/FIO" || apis[0].Header != "Authorization" {
		t.Fatalf("state %+v", apis)
	}
	maps := AddAPIInput{Project: "finance", Name: "maps", BaseURL: "https://maps.googleapis.com",
		Auth: APIAuthInput{Credential: "GOOGLE_MAPS", QueryParam: "key"}, Methods: []string{"GET"}}
	cMaps := tr.render(VerbAddAPI, maps)
	tr.mustClass(cMaps, Widening)
	mapsBlock := cMaps.Ops[0].Content[strings.LastIndex(cMaps.Ops[0].Content, `name: "maps"`):]
	if !strings.Contains(mapsBlock, `query_param: "key"`) || strings.Contains(mapsBlock, `header: "Authorization"`) {
		t.Fatalf("query-param auth rendered incorrectly:\n%s", cMaps.Ops[0].Content)
	}
	tr.apply(cMaps)
	apis = tr.state().Projects["hermes--finance"].APIs
	if got := apis[1]; got.QueryParam != "key" || got.Header != "" || got.AuthRef != "secret://hermes/GOOGLE_MAPS" {
		t.Fatalf("query-param API state %+v", got)
	}

	// A role may now hold query_api; the credential can be requested.
	if c := tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{{Name: "worker", Instructions: "x", Tools: []string{"query_api"}}}}); c.Class == Refused {
		t.Fatalf("query_api refused with an approved API: %s", c.Reason)
	}
	if c := tr.render(VerbRequestCredential, RequestCredentialInput{Project: "finance", Name: "FIO", Purpose: "x", Kind: "secret"}); c.Class != Widening {
		t.Fatalf("the API's credential cannot be requested: %s", c.Reason)
	}

	// Writes are proposable and named.
	w := fioAPI()
	w.Name, w.Methods, w.Writes = "pay", []string{"GET", "POST"}, true
	cw := tr.render(VerbAddAPI, w)
	tr.mustClass(cw, Widening)
	if strings.Join(cw.Grant.Integrations[0].Write, ",") != "POST" || !strings.Contains(cw.Sentence, "approval before it is made") {
		t.Fatalf("write grant %+v / %s", cw.Grant.Integrations[0], cw.Sentence)
	}

	for name, mutate := range map[string]func(*AddAPIInput){
		"http public":        func(a *AddAPIInput) { a.BaseURL = "http://api.example" },
		"query in base":      func(a *AddAPIInput) { a.BaseURL = "https://api.example/?x=1" },
		"no methods":         func(a *AddAPIInput) { a.Methods = nil },
		"unknown method":     func(a *AddAPIInput) { a.Methods = []string{"TRACE"} },
		"write w/o writes":   func(a *AddAPIInput) { a.Methods = []string{"DELETE"} },
		"writes w/o a write": func(a *AddAPIInput) { a.Writes = true },
		"bad credential":     func(a *AddAPIInput) { a.Auth.Credential = "fio" },
		"reserved header":    func(a *AddAPIInput) { a.Auth.Header = "Cookie" },
		"bad query param":    func(a *AddAPIInput) { a.Auth.QueryParam = "bad key" },
		"two placements":     func(a *AddAPIInput) { a.Auth.QueryParam = "key" },
		"query with prefix":  func(a *AddAPIInput) { a.Auth.Header, a.Auth.QueryParam = "", "key" },
		"name taken":         func(*AddAPIInput) {},
		"write suffix":       func(a *AddAPIInput) { a.Name = "fio2-write" },
		"unknown project":    func(a *AddAPIInput) { a.Project = "nope" },
	} {
		in := fioAPI()
		in.Methods = append([]string(nil), in.Methods...)
		mutate(&in)
		if c := tr.render(VerbAddAPI, in); c.Class != Refused {
			t.Errorf("%s: accepted", name)
		}
	}

	// Removal withdraws the approval and removes the API.
	rm := tr.render(VerbRemove, RemoveInput{Kind: "integration", Project: "finance", ID: "fio"})
	tr.mustClass(rm, Inert)
	tr.apply(rm)
	for _, a := range tr.state().Projects["hermes--finance"].APIs {
		if a.Name == "fio" {
			t.Fatal("the removed API survived")
		}
	}
}

// query_api is refused for a role while the project has no approved API.
func TestDefineSwarm_QueryAPINeedsAnApprovedAPI(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	if c := tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{{Name: "worker", Instructions: "x", Tools: []string{"query_api"}}}}); c.Class != Refused {
		t.Fatal("query_api allowed with no API")
	}
}

// Design §7.6: a workflow whose role holds query_api reaches every API of
// its project, with their credentials; adding an API changes its reach.
// Control: addAPIReach.
func TestReach_QueryAPIBindsTheProjectAPIs(t *testing.T) {
	p := &registry.Project{ID: "hermes--fin", APIs: []registry.ProjectAPI{{Name: "fio", BaseURL: "https://a.example",
		Methods: []string{"GET"}, Auth: registry.ProjectAPIAuth{ValueFrom: "secret://hermes/FIO"}}}}
	sw := &registry.Swarm{Roles: []registry.SwarmRole{{Name: "worker", Permissions: registry.SwarmRolePermissions{AllowedTools: []string{"query_api"}}}}}
	wf := &registry.Workflow{ID: "hermes--fin--w", Broker: &registry.WorkflowBroker{}, Steps: map[string]registry.WorkflowStep{"s": {Type: "agent", Role: "worker"}}}
	sig, err := SignatureOf(p, sw, wf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sig.Integrations, ",") != "api:fio" || strings.Join(sig.Credentials, ",") != "api:fio=secret://hermes/FIO" {
		t.Fatalf("signature %+v", sig)
	}
	p.APIs = append(p.APIs, registry.ProjectAPI{Name: "pay", BaseURL: "https://b.example", Methods: []string{"GET"}})
	sig2, _ := SignatureOf(p, sw, wf)
	if sig2.Hash() == sig.Hash() {
		t.Fatal("a new API did not change the reach of a query_api workflow")
	}
}
