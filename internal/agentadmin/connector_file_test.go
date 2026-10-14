package agentadmin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// GitHub #314, operator decision 2026-10-13 (vet the three shipped
// recipes; agent-administered design §19.14.11): the operator vets a
// recipe in an operator-owned connector.yaml beside it, under the same
// loader rules as an inline block; the release ships an example without a
// URL, because no server offering the recipes' tools is in evidence.

const operatorConnector = `title: "Calendar (read)"
servers:
  calendar: {url: "https://calendar-mcp.example/mcp"}
vetted: {by: "operator", at: "2026-10-13", recipe_version: 1}
`

// withOperatorFile is the shipped tree plus recipes/<name>/connector.yaml.
func withOperatorFile(t *testing.T, recipes map[string]string, name, body string) fstest.MapFS {
	t.Helper()
	fsys := recipeFS(t, "", recipes)
	fsys["recipes/"+name+"/connector.yaml"] = &fstest.MapFile{Data: []byte(body)}
	return fsys
}

func TestConnectorLoad_OperatorFileVetsARecipe(t *testing.T) {
	r, err := NewRenderer(withOperatorFile(t, map[string]string{"agenda": shippedRecipe(t, "agenda")}, "agenda", operatorConnector))
	if err != nil {
		t.Fatal(err)
	}
	got := r.Catalogue().Connectors()
	if len(got) != 1 || got[0].Name != "agenda" || got[0].Connector.Servers["calendar"].URL != connectorURL {
		t.Fatalf("connectors %+v", got)
	}
	// The same rules: a stale vetting demotes, an unknown key or a missing
	// server refuses the catalogue.
	stale, err := NewRenderer(withOperatorFile(t, map[string]string{"agenda": shippedRecipe(t, "agenda")}, "agenda",
		strings.Replace(operatorConnector, "recipe_version: 1", "recipe_version: 2", 1)))
	if err != nil || len(stale.Catalogue().Connectors()) != 0 {
		t.Fatalf("a stale operator vetting: %v", err)
	}
	for name, body := range map[string]string{
		"unknown key":    operatorConnector + "extra: true\n",
		"missing server": strings.Replace(operatorConnector, "calendar: {", "other: {", 1),
		"not https":      strings.Replace(operatorConnector, "https://", "http://", 1),
		"not yaml":       "title: [",
	} {
		if _, err := NewRenderer(withOperatorFile(t, map[string]string{"agenda": shippedRecipe(t, "agenda")}, "agenda", body)); err == nil ||
			!strings.Contains(err.Error(), "connector") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Two vettings of one recipe are refused.
func TestConnectorLoad_RecipeBlockAndOperatorFileRefused(t *testing.T) {
	_, err := NewRenderer(withOperatorFile(t, map[string]string{"agenda": agendaConnector(t)}, "agenda", operatorConnector))
	if err == nil || !strings.Contains(err.Error(), "connector.yaml") {
		t.Fatalf("a recipe vetted twice: %v", err)
	}
}

// §19.14.11: the shipped catalogue offers no connector, each shipped recipe
// carries an example vetting file without a URL, and the example becomes a
// connector once an operator fills its URLs.
func TestShippedRecipes_ShipNoVettingAndSayWhy(t *testing.T) {
	r, err := NewRenderer(os.DirFS("../../configs/agent-templates"))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Catalogue().Connectors(); len(got) != 0 {
		t.Fatalf("the release ships a vetted connector: %v", got[0].Name)
	}
	for _, name := range []string{"agenda", "inbox-digest", "morning-brief"} {
		raw, err := os.ReadFile(filepath.Join("../../configs/agent-templates/recipes", name, "connector.yaml.example"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		example := string(raw)
		if !strings.Contains(example, "§19.14.11") || strings.Contains(example, "https://") {
			t.Fatalf("%s: the example must cite the evidence and name no URL:\n%s", name, example)
		}
		filled := strings.ReplaceAll(example, `url: ""`, `url: "https://`+name+`-mcp.example/mcp"`)
		filled = strings.ReplaceAll(filled, `by: ""`, `by: "operator"`)
		filled = strings.ReplaceAll(filled, `at: ""`, `at: "2026-10-13"`)
		got, err := NewRenderer(withOperatorFile(t, map[string]string{name: shippedRecipe(t, name)}, name, filled))
		if err != nil {
			t.Fatalf("%s: the filled example does not load: %v", name, err)
		}
		if c := got.Catalogue().Connectors(); len(c) != 1 || c[0].Name != name {
			t.Fatalf("%s: the filled example is not a connector (%q)", name, got.Catalogue().Get(name).ConnectorDemoted())
		}
	}
}
