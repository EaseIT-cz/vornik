package agentadmin

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
)

func metadataState() *State {
	source := []byte("projectId: hermes--news\ndisplayName: Original\ndescription: Original purpose\n# retain\nbudget:\n  monthly_hard_usd: 2\n")
	return &State{Namespace: "hermes", Projects: map[string]*ProjectState{"hermes--news": {ID: "hermes--news"}},
		FileHashes: map[string]string{"projects/hermes--news.yaml": HashContent(source)}, ProjectYAML: map[string][]byte{"projects/hermes--news.yaml": source}}
}

// Issue #67: metadata changes are inert and preserve native sibling fields,
// immutable identity and omitted descriptive values.
func TestUpdateProjectMetadata(t *testing.T) {
	for _, raw := range []string{`{"project":"news","purpose":"Updated"}`, `{"project":"news","display_name":"Hermes: News"}`, `{"project":"news","purpose":"Quotes: \"quoted\"","display_name":"Hermes: News"}`} {
		st := metadataState()
		c, err := (&Renderer{}).Render(st, VerbUpdateProject, json.RawMessage(raw))
		if err != nil || c.Class != Inert || len(c.Ops) != 1 || c.Ops[0].Op != OpReplace {
			t.Fatalf("change %+v: %v", c, err)
		}
		var got map[string]any
		if err := yaml.Unmarshal([]byte(c.Ops[0].Content), &got); err != nil {
			t.Fatal(err)
		}
		if got["projectId"] != "hermes--news" || !strings.Contains(c.Ops[0].Content, "# retain") || !strings.Contains(c.Ops[0].Content, "monthly_hard_usd: 2") {
			t.Fatalf("sibling fields lost: %s", c.Ops[0].Content)
		}
		if len(c.Grant.Workflows) != 0 || len(c.Narrow.RemovedProjects) != 0 || c.ReadSet[c.Ops[0].Path] != st.FileHashes[c.Ops[0].Path] {
			t.Fatalf("reach or readset altered: %+v", c)
		}
	}
}

func TestUpdateProjectMetadataRefusals(t *testing.T) {
	for _, raw := range []string{`{`, `{"project":"news","purpose":"ok","slug":"rename"}`, `{"project":"other--news","purpose":"x"}`, `{"project":"missing","purpose":"x"}`, `{"project":"news"}`, `{"project":"news","purpose":null}`, `{"project":"news","display_name":null}`, `{"project":"news","purpose":3}`, `{"project":"news","purpose":"a\nb"}`, `{"project":"news","display_name":" "}`, `{"project":"news","purpose":"` + strings.Repeat("x", 301) + `"}`} {
		c, err := (&Renderer{}).Render(metadataState(), VerbUpdateProject, json.RawMessage(raw))
		if err != nil || c.Class != Refused || len(c.Ops) != 0 {
			t.Fatalf("accepted %s: %+v %v", raw, c, err)
		}
	}
	for _, mode := range []string{"missing", "hash mismatch", "malformed", "sequence", "empty", "locked", "multiple docs", "anchor"} {
		t.Run(mode, func(t *testing.T) {
			st := metadataState()
			path := "projects/hermes--news.yaml"
			switch mode {
			case "missing":
				delete(st.ProjectYAML, path)
			case "hash mismatch":
				st.FileHashes[path] = "wrong"
			case "malformed":
				st.ProjectYAML[path] = []byte("x: [")
			case "sequence":
				st.ProjectYAML[path] = []byte("- item")
			case "empty":
				st.ProjectYAML[path] = nil
			case "multiple docs":
				st.ProjectYAML[path] = []byte("projectId: hermes--news\n---\nother: value")
			case "anchor":
				st.ProjectYAML[path] = []byte("projectId: hermes--news\ndescription: &shared Original\ndisplayName: *shared\n")
			case "locked":
				st.Locked = map[string]bool{lockProject("hermes--news"): true}
			}
			if mode == "malformed" || mode == "sequence" || mode == "empty" || mode == "multiple docs" || mode == "anchor" {
				st.FileHashes[path] = HashContent(st.ProjectYAML[path])
			}
			c, err := (&Renderer{}).Render(st, VerbUpdateProject, json.RawMessage(`{"project":"news","purpose":"Updated"}`))
			if err != nil || c.Class != Refused {
				t.Fatalf("accepted %s: %+v %v", mode, c, err)
			}
		})
	}
}

func TestPatchProjectMetadataAddsMissingNativeFields(t *testing.T) {
	got, err := patchProjectMetadata([]byte("projectId: hermes--news\n"), map[string]string{"description": "Purpose", "displayName": "Name"})
	if err != nil || !strings.Contains(got, `description: "Purpose"`) || !strings.Contains(got, `displayName: "Name"`) {
		t.Fatalf("patched %q: %v", got, err)
	}
}

// Issue #71: agent-admin roles can render a document without broad shell reach.
func TestDefineSwarmGrantsDocumentRendererButRefusesShell(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("news")
	for _, tool := range []string{"document_render", "run_shell"} {
		change := tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "news", Roles: []RoleInput{{Name: "worker", Instructions: "Make a report", Tools: []string{tool}}}})
		if tool == "document_render" {
			tr.mustClass(change, Inert)
		} else if change.Class != Refused {
			t.Fatal("broad shell reach granted")
		}
	}
}
