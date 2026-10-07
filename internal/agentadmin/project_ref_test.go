package agentadmin

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// BACKLOG 2026-10-03: define_workflow refused claudecode--engineering as
// "claudecode--claudecode--engineering". Review focus 4: strip once.
func TestProjectSlug_StripsOnce(t *testing.T) {
	for in, want := range map[string]string{
		"engineering":             "engineering",
		"claudecode--claudecode":  "claudecode",
		"claudecode":              "claudecode",
		"claudecode--engineering": "engineering",
		"other--x":                "other--x",
	} {
		if got := projectSlug("claudecode", in); got != want {
			t.Errorf("projectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every verb that names an existing project gives the same answer for the
// slug and for the full id, and refuses another namespace's id.
func TestVerbs_AcceptAProjectsFullID(t *testing.T) {
	type call struct {
		verb string
		in   func(ref string) any
		tr   func(t *testing.T) *tree
	}
	finance := func(t *testing.T) *tree {
		tr := newTree(t, "hermes")
		tr.project("finance")
		tr.approveServer("finance", "fio", "https://api.fio.example/mcp", "balance")
		tr.apply(tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{
			{Name: "reader", Instructions: "Read.", Tools: []string{"file_write", "mcp__fio__balance"}}}}))
		return tr
	}
	calls := []call{
		{VerbDefineSwarm, func(ref string) any {
			return DefineSwarmInput{Slug: ref, Roles: []RoleInput{{Name: "w", Instructions: "Write.", Tools: []string{"file_write"}}}}
		}, finance},
		{VerbDefineWorkflow, func(ref string) any {
			return DefineWorkflowInput{Project: ref, Slug: "spend", Purpose: "spending",
				Steps:  []StepInput{{Name: "read", Role: "reader", Instructions: "Summarise."}},
				Inputs: []byte(`{"type":"object","additionalProperties":false,"properties":{}}`), Egress: egress1()}
		}, finance},
		{VerbAddMCPServer, func(ref string) any {
			return AddMCPServerInput{Project: ref, Name: "mail", URL: "https://mail.example/mcp", Auth: MCPAuthInput{Mode: "none"}}
		}, finance},
		{VerbSetBudget, func(ref string) any { return SetBudgetInput{Project: ref, MonthlyUSD: 1.5} }, finance},
		{VerbAddAPI, func(ref string) any { a := fioAPI(); a.Project = ref; a.Name = "bank"; return a }, finance},
		{VerbRequestCredential, func(ref string) any {
			return RequestCredentialInput{Project: ref, Name: "FIO_TOKEN", Purpose: "read the balance", Kind: "secret"}
		}, finance},
		{VerbRemove, func(ref string) any { return RemoveInput{Kind: "project", ID: ref} }, finance},
		{VerbRemove, func(ref string) any { return RemoveInput{Kind: "integration", Project: ref, ID: "fio"} }, finance},
		{VerbInstallRecipe, func(ref string) any { v := shippedVars("inbox-digest"); v.Project = ref; return v }, func(t *testing.T) *tree {
			tr := shippedTree(t)
			tr.project("finance")
			return tr
		}},
	}
	for _, c := range calls {
		t.Run(c.verb, func(t *testing.T) {
			tr := c.tr(t)
			slug := "finance"
			if c.verb == VerbInstallRecipe {
				slug = "personal"
			}
			bare := tr.render(c.verb, c.in(slug))
			if bare.Class == Refused {
				t.Fatalf("the bare-slug control is refused: %s", bare.Reason)
			}
			full := tr.render(c.verb, c.in("hermes--"+slug))
			if full.Class == Refused || full.Class != bare.Class || full.Sentence != bare.Sentence || len(full.Ops) != len(bare.Ops) {
				t.Fatalf("full id: class %s (%s), sentence %q; slug: class %s, sentence %q", full.Class, full.Reason, full.Sentence, bare.Class, bare.Sentence)
			}
			for i := range bare.Ops {
				if full.Ops[i].Path != bare.Ops[i].Path {
					t.Fatalf("op %d targets %s, want %s", i, full.Ops[i].Path, bare.Ops[i].Path)
				}
			}
			if other := tr.render(c.verb, c.in("other--"+slug)); other.Class != Refused {
				t.Fatalf("another namespace's id was accepted: %s", other.Class)
			}
		})
	}
}

// remove workflow keeps <project>--<workflow>; the hint fires only for a
// three-part id.
func TestRemove_WorkflowHint(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	hint := `pass the workflow as "finance--spend"`
	c := tr.render(VerbRemove, RemoveInput{Kind: "workflow", ID: "hermes--finance--spend"})
	if c.Class != Refused || !strings.Contains(c.Reason, hint) || strings.Contains(c.Reason, "hermes--hermes") {
		t.Fatalf("full workflow id: %s %q", c.Class, c.Reason)
	}
	for _, id := range []string{"finance--spend", "hermes--w", "other--a--b"} { // hermes--w: project equals the namespace; other--a--b: another namespace
		c = tr.render(VerbRemove, RemoveInput{Kind: "workflow", ID: id})
		if c.Class != Refused || strings.Contains(c.Reason, "without the namespace") || !strings.Contains(c.Reason, "there is no workflow") {
			t.Fatalf("%s: %s %q", id, c.Class, c.Reason)
		}
	}
}

// Review 1843 item 3, hardened by review a8bf: a project read by raw caller
// input would bypass the namespace. Every IndexExpr on a field named Projects,
// whatever the receiver, sits in a reviewed function that composes the key with
// agentns.ID, checks agentns.FromID, or derives it from a composed workflow id.
// (A range over Projects is not keyed by input and is not an index.)
func TestProjectReads_AreNamespaceGuarded(t *testing.T) {
	known := map[string]string{
		"createProject": "composes the id", "defineSwarm": "composes the id", "defineWorkflow": "composes the id",
		"addMCPServer": "composes the id", "setBudget": "composes the id", "addAPI": "composes the id",
		"requestCredential": "composes the id", "updateProject": "composes the id", "planInstall": "composes the id",
		"approveServerTools": "checks FromID == ns", "removeWorkflow": "owner derived from a composed workflow id",
		"removeProject": "takes an already composed id", "removeIntegration": "takes an already composed id",
	}
	entries, _ := os.ReadDir(".")
	seen := map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, _ := os.ReadFile(e.Name())
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, e.Name(), src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			indexes := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if ix, ok := n.(*ast.IndexExpr); ok {
					if sel, ok := ix.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Projects" {
						indexes = true
					}
				}
				return true
			})
			if !indexes {
				continue
			}
			seen[fn.Name.Name] = true
			body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			if _, ok := known[fn.Name.Name]; !ok {
				t.Errorf("%s indexes Projects and is not on the reviewed list: guard its key with agentns.ID or agentns.FromID, then list it", fn.Name.Name)
			}
			if !strings.Contains(body, "agentns.ID(") && !strings.Contains(body, "agentns.FromID(") && !strings.Contains(body, "ProjectOfWorkflow(") &&
				known[fn.Name.Name] != "takes an already composed id" {
				t.Errorf("%s indexes Projects without composing or checking the namespace", fn.Name.Name)
			}
		}
	}
	for name := range known {
		if !seen[name] {
			t.Errorf("%s is listed but no longer indexes Projects: drop it from the list", name)
		}
	}
}

// update_project works on a project file's text, so it has its own fixture.
func TestUpdateProject_FullID(t *testing.T) {
	run := func(ref string) Change {
		c, err := (&Renderer{}).Render(metadataState(), VerbUpdateProject, json.RawMessage(`{"project":"`+ref+`","purpose":"Updated"}`))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	bare, full := run("news"), run("hermes--news")
	if full.Class != Inert || full.Class != bare.Class || len(full.Ops) != 1 || full.Ops[0].Path != bare.Ops[0].Path {
		t.Fatalf("full id: %s %s %+v; slug: %s", full.Class, full.Reason, full.Ops, bare.Class)
	}
	if c := run("other--news"); c.Class != Refused {
		t.Fatalf("another namespace's id: %s", c.Class)
	}
}
