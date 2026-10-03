package agentadmin

import (
	"os"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Agent-administered design §18.11 (2026-10-03): a role's instructions never
// reached the role. The template wrote a layout the parser does not read, and
// the read-back dropped the instructions, so a re-render blanked them in the
// file too. These pin both halves through the real template and parser.

func roleRoundTripState() (*State, *ProjectState) {
	p := &ProjectState{ID: "claudecode--docreview", DisplayName: "docreview",
		Swarm: SwarmState{ID: "claudecode--docreview", Roles: []RoleSpec{
			{Name: "reviewer", Description: "Reviews a design",
				Instructions: "You review a design.\n\nQuote what you object to.", Tools: []string{"file_read", "file_write"}},
			{Name: "worker", Description: "Does one step",
				Instructions: "Do the step you are given.", Tools: []string{"file_read", "file_write"}},
		}}}
	return &State{Namespace: "claudecode", AgentImage: "img"}, p
}

func renderAndParse(t *testing.T, r *Renderer, st *State, p *ProjectState) *registry.Swarm {
	t.Helper()
	out, err := r.renderSwarm(st, p)
	if err != nil {
		t.Fatal(err)
	}
	sw, err := registry.ParseSwarmMarkdown([]byte(out), p.Swarm.ID+".md")
	if err != nil {
		t.Fatalf("rendered swarm does not parse: %v\n%s", err, out)
	}
	return sw
}

func TestRenderedSwarm_EachRoleLoadsItsInstructions(t *testing.T) {
	r, err := NewRenderer(os.DirFS("../../configs/agent-templates"))
	if err != nil {
		t.Fatal(err)
	}
	st, p := roleRoundTripState()
	sw := renderAndParse(t, r, st, p)
	for i, want := range p.Swarm.Roles {
		if got := sw.Roles[i].SystemPrompt; got != want.Instructions {
			t.Errorf("role %q loads system prompt %q, want its instructions %q", want.Name, got, want.Instructions)
		}
	}
}

// A verb that re-renders the swarm starts from the loaded registry; it must
// write every role back with the same instructions.
func TestReadBackThenRerender_KeepsInstructions(t *testing.T) {
	r, err := NewRenderer(os.DirFS("../../configs/agent-templates"))
	if err != nil {
		t.Fatal(err)
	}
	st, p := roleRoundTripState()
	loaded := renderAndParse(t, r, st, p)
	back := ProjectStateFrom(&registry.Project{ID: p.ID}, loaded)
	back.DisplayName = p.DisplayName
	back.Swarm.ID = p.Swarm.ID
	for i, want := range p.Swarm.Roles {
		if got := back.Swarm.Roles[i].Instructions; got != want.Instructions {
			t.Errorf("read-back of role %q has instructions %q, want %q", want.Name, got, want.Instructions)
		}
	}
	again := renderAndParse(t, r, st, back)
	for i, want := range p.Swarm.Roles {
		if got := again.Roles[i].SystemPrompt; got != want.Instructions {
			t.Errorf("after a re-render role %q loads %q, want %q", want.Name, got, want.Instructions)
		}
	}
}

// §18.11 item 4: a recipe install re-renders the project's swarm. Before the
// fix, the read-back carried no instructions and the install fell back to
// each role's one-line description, so installing a recipe shrank every
// other role's text; the file it wrote was also in the layout the parser
// ignored.
func TestInstallRecipe_KeepsOtherRolesFullInstructions(t *testing.T) {
	tr := shippedTree(t)
	text := "Read the mail.\nThen file a short summary for the person."
	tr.apply(tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "personal", Roles: []RoleInput{
		{Name: "reader", Instructions: text, Tools: []string{"file_read"}}}}))
	tr.apply(tr.render(VerbInstallRecipe, shippedVars("inbox-digest")))

	path := "swarms/hermes--personal.md"
	content, ok := tr.files[path]
	if !ok {
		t.Fatalf("no %s; files: %v", path, keysOf(tr.files))
	}
	if !strings.Contains(content, "\n## Role prompts\n") {
		t.Errorf("the re-rendered swarm is not in the parser's layout:\n%s", content)
	}
	sw, err := registry.ParseSwarmMarkdown([]byte(content), path)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range sw.Roles {
		if r.Name == "reader" && r.SystemPrompt != text {
			t.Errorf("after the install the reader loads %q, want %q", r.SystemPrompt, text)
		}
	}
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
