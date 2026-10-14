package agentadmin

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// GitHub #314 (2026-10-13): read-only connectors a paired device installs
// (agent-administered design §19.14, build decisions §19.14.8). The loader
// rules (§19.14.1 rules 1 to 4, §19.14.8 items 1 to 4), the list_recipes
// marker and the device path's install input.

const connectorURL = "https://calendar-mcp.example/mcp"

// shippedRecipe reads a shipped recipe.yaml.
func shippedRecipe(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../configs/agent-templates/recipes/" + name + "/recipe.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// withConnector appends a connector block to a recipe.
func withConnector(recipe, block string) string {
	return strings.TrimRight(recipe, "\n") + "\n" + block
}

// agendaConnector is the shipped agenda with a vetted connector block.
func agendaConnector(t *testing.T) string {
	return withConnector(shippedRecipe(t, "agenda"), `connector:
  title: "Calendar (read)"
  servers:
    calendar: {url: "`+connectorURL+`"}
  vetted: {by: "release reviewer", at: "2026-10-13", recipe_version: 1}
`)
}

func catalogueWith(t *testing.T, recipes map[string]string) (*Catalogue, error) {
	t.Helper()
	r, err := NewRenderer(recipeFS(t, "", recipes))
	if err != nil {
		return nil, err
	}
	return r.Catalogue(), nil
}

func TestConnectorLoad_AVettedRecipeIsAConnector(t *testing.T) {
	cat, err := catalogueWith(t, map[string]string{"agenda": agendaConnector(t)})
	if err != nil {
		t.Fatal(err)
	}
	got := cat.Connectors()
	if len(got) != 1 || got[0].Name != "agenda" || got[0].Connector.Title != "Calendar (read)" {
		t.Fatalf("connectors: %+v", got)
	}
	if why := got[0].ConnectorDemoted(); why != "" {
		t.Fatalf("demoted: %s", why)
	}
}

// Rule 1 (§19.14.8 item 1): the person never types an endpoint. A need with
// no vetted server, a server for no need, a url variable nothing binds, a
// literal URL that differs from its server and a variable used as a server
// URL each refuse the whole catalogue.
func TestConnectorLoad_RefusesURLVariable(t *testing.T) {
	agenda := shippedRecipe(t, "agenda")
	brief := shippedRecipe(t, "morning-brief")
	cases := map[string]struct{ name, recipe, want string }{
		"need without a server": {"morning-brief", withConnector(brief, `connector:
  title: "Brief"
  servers:
    calendar: {url: "`+connectorURL+`"}
  vetted: {by: "r", at: "2026-10-13", recipe_version: 1}
`), "mail"},
		"server for no need": {"agenda", withConnector(agenda, `connector:
  title: "Calendar"
  servers:
    calendar: {url: "`+connectorURL+`"}
    other: {url: "https://other.example/mcp"}
  vetted: {by: "r", at: "2026-10-13", recipe_version: 1}
`), "other"},
		"server URL is a variable": {"agenda", withConnector(agenda, `connector:
  title: "Calendar"
  servers:
    calendar: {url: "{{calendar_server_url}}"}
  vetted: {by: "r", at: "2026-10-13", recipe_version: 1}
`), "variable"},
		"server URL not https": {"agenda", withConnector(agenda, `connector:
  title: "Calendar"
  servers:
    calendar: {url: "http://calendar.example/mcp"}
  vetted: {by: "r", at: "2026-10-13", recipe_version: 1}
`), "calendar"},
		"literal need URL differs": {"agenda", withConnector(strings.Replace(strings.Replace(agenda, `url: "{{calendar_server_url}}"`, `url: "https://a.example/mcp"`, 1),
			`  calendar_server_url: {type: url, help: "The MCP server that reads your calendar (it must offer calendar_list_events)."}`+"\n", "", 1), `connector:
  title: "Calendar"
  servers:
    calendar: {url: "`+connectorURL+`"}
  vetted: {by: "r", at: "2026-10-13", recipe_version: 1}
`), "calendar"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := catalogueWith(t, map[string]string{tc.name: tc.recipe})
			if err == nil || !strings.Contains(err.Error(), "connector") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load error %v, want a connector refusal naming %q", err, tc.want)
			}
		})
	}
}

// Rule 2 (§19.14.8 item 3): a connector's other variables are enum, cron
// or the schedule's timezone string; any other string is refused, and so is
// an SSE transport (item 2).
func TestConnectorLoad_RefusesFreeStringAndSSE(t *testing.T) {
	digest := shippedRecipe(t, "inbox-digest")
	block := `connector:
  title: "Mail digest"
  servers:
    mail: {url: "https://mail-mcp.example/mcp"}
  vetted: {by: "r", at: "2026-10-13", recipe_version: 1}
`
	if _, err := catalogueWith(t, map[string]string{"inbox-digest": withConnector(digest, block)}); err != nil {
		t.Fatalf("cron and timezone variables are allowed: %v", err)
	}
	_, err := catalogueWith(t, map[string]string{"probe": withConnector(probeRecipe, `connector:
  title: "Probe"
  servers:
    src: {url: "https://src.example/mcp"}
  vetted: {by: "r", at: "2026-10-13", recipe_version: 1}
`)})
	if err == nil || !strings.Contains(err.Error(), "topic") {
		t.Fatalf("a free string variable on a connector: %v", err)
	}
	_, err = catalogueWith(t, map[string]string{"inbox-digest": withConnector(digest, strings.Replace(block, `/mcp"}`, `/mcp", transport: sse}`, 1))})
	if err == nil || !strings.Contains(err.Error(), "streamable-http") {
		t.Fatalf("an sse connector: %v", err)
	}
}

// Rule 4: a recipe edited after vetting is no longer a connector.
func TestConnectorLoad_StaleVettingIsNotAConnector(t *testing.T) {
	stale := strings.Replace(agendaConnector(t), "recipe_version: 1", "recipe_version: 2", 1)
	r, err := NewRenderer(recipeFS(t, "", map[string]string{"agenda": stale}))
	if err != nil {
		t.Fatalf("a stale vetting refused the catalogue: %v", err)
	}
	if got := r.Catalogue().Connectors(); len(got) != 0 {
		t.Fatalf("a stale connector is listed: %+v", got)
	}
	if why := r.Catalogue().Get("agenda").ConnectorDemoted(); !strings.Contains(why, "version") {
		t.Fatalf("demotion reason %q", why)
	}
	tr := shippedTree(t)
	tr.r = r
	for _, v := range r.ListRecipes(tr.state()) {
		if v.Connector {
			t.Fatalf("list_recipes marks a stale connector: %+v", v)
		}
	}
}

// Rule 4 demotes only the connector: the agent path installs it as before.
func TestConnectorLoad_StaleVettingStillInstallsAsRecipe(t *testing.T) {
	stale := strings.Replace(agendaConnector(t), "recipe_version: 1", "recipe_version: 2", 1)
	tr := shippedTree(t)
	tr.r = rendererWith(t, "", map[string]string{"agenda": stale})
	tr.mustClass(tr.render(VerbInstallRecipe, shippedVars("agenda")), Widening)
}

// §19.14.10 (superseding §19.14.8 item 4's demotion): an OAuth need is
// offered; the device installs it, signs in, then approves the tools.
func TestConnectorLoad_OAuthIsOffered(t *testing.T) {
	oauth := strings.Replace(agendaConnector(t), "credential: {name: CALENDAR_TOKEN, kind: secret}", "credential: {name: OAUTH_CALENDAR, kind: oauth}", 1)
	cat, err := catalogueWith(t, map[string]string{"agenda": oauth})
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Connectors()) != 1 || cat.Get("agenda").ConnectorDemoted() != "" {
		t.Fatalf("an oauth connector is not offered: %q", cat.Get("agenda").ConnectorDemoted())
	}
}

// The agent path is unchanged by a connector block: the same change, the
// agent's own URL.
func TestInstallRecipe_AgentPathUnchangedForAConnectorRecipe(t *testing.T) {
	plain := shippedTree(t)
	withBlock := shippedTree(t)
	withBlock.r = rendererWith(t, "", map[string]string{"agenda": agendaConnector(t)})
	a := plain.render(VerbInstallRecipe, shippedVars("agenda"))
	b := withBlock.render(VerbInstallRecipe, shippedVars("agenda"))
	withBlock.mustClass(b, Widening)
	if a.Sentence != b.Sentence || len(a.Ops) != len(b.Ops) {
		t.Fatalf("sentence or ops differ:\n%s\n%s", a.Sentence, b.Sentence)
	}
	for i := range a.Ops {
		if a.Ops[i].Content != b.Ops[i].Content {
			t.Fatalf("op %s differs", a.Ops[i].Path)
		}
	}
	if strings.Contains(b.Sentence, connectorURL) {
		t.Fatal("the agent path used the vetted URL")
	}
	for _, v := range withBlock.r.ListRecipes(withBlock.state()) {
		if v.Name == "agenda" && !v.Connector {
			t.Fatal("list_recipes does not mark the connector")
		}
	}
}

// The device path's install input binds the vetted URL and takes only the
// device-fillable variables: a forged URL variable is ignored.
func TestConnectorInput_BindsTheVettedURLAndIgnoresForgedFields(t *testing.T) {
	tr := shippedTree(t)
	tr.r = rendererWith(t, "", map[string]string{"agenda": agendaConnector(t)})
	raw, why := tr.r.ConnectorInstallInput("agenda", "personal", "work", map[string]string{
		"calendar_server_url": "https://evil.example/mcp",
	})
	if why != "" {
		t.Fatal(why)
	}
	var in InstallRecipeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	if string(in.Variables["calendar_server_url"]) != `"`+connectorURL+`"` || in.Account != "work" || in.Project != "personal" {
		t.Fatalf("input %s", raw)
	}
	c := tr.render(VerbInstallRecipe, in)
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Sentence, connectorURL) || strings.Contains(c.Sentence, "evil") || !strings.Contains(c.Sentence, "work") {
		t.Fatalf("sentence %s", c.Sentence)
	}
	if _, why := tr.r.ConnectorInstallInput("inbox-digest", "personal", "", nil); why == "" {
		t.Fatal("a recipe that is not a connector was accepted")
	}
}

// Rule 2 at confirm: the timezone is an IANA zone, a cron is a cron.
func TestConnectorInput_ChecksDeviceVariables(t *testing.T) {
	digest := withConnector(shippedRecipe(t, "inbox-digest"), `connector:
  title: "Mail digest"
  servers:
    mail: {url: "https://mail-mcp.example/mcp"}
  vetted: {by: "r", at: "2026-10-13", recipe_version: 1}
`)
	r := rendererWith(t, "", map[string]string{"inbox-digest": digest})
	if _, why := r.ConnectorInstallInput("inbox-digest", "personal", "", map[string]string{"timezone": "Mars/Olympus"}); !strings.Contains(why, "timezone") {
		t.Fatalf("a bad zone: %q", why)
	}
	if _, why := r.ConnectorInstallInput("inbox-digest", "personal", "", map[string]string{"digest_cron": "every day"}); !strings.Contains(why, "cron") {
		t.Fatalf("a bad cron: %q", why)
	}
	if _, why := r.ConnectorInstallInput("inbox-digest", "personal", "", map[string]string{"timezone": "Europe/Prague"}); why != "" {
		t.Fatal(why)
	}
	vars := r.Catalogue().Get("inbox-digest").DeviceVariables()
	if len(vars) != 3 || vars[0].Name != "digest_cron" || vars[1].Name != "since_default" || vars[2].Name != "timezone" {
		t.Fatalf("device variables %+v", vars)
	}
}
