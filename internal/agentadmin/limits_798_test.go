package agentadmin

// GitHub #798 and #805 (part 06 §6.9). Found at: production (the #793 case
// study, built with the admin tools only: four workflows ran one after
// another, and a reviewer could not read a 77 KB diff in 30 tool steps).
// Tests at: integration (the real templates, rendered, parsed back by the
// registry, and re-rendered by another verb).

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func concurrency798(tr *tree, project string) int {
	tr.t.Helper()
	return tr.state().Projects["hermes--"+project].MaxConcurrentTasks
}

func role798(tr *tree, project, name string) RoleSpec {
	tr.t.Helper()
	for _, r := range tr.state().Projects["hermes--"+project].Swarm.Roles {
		if r.Name == name {
			return r
		}
	}
	tr.t.Fatalf("no role %s", name)
	return RoleSpec{}
}

func updateConcurrency798(tr *tree, n any) Change {
	tr.t.Helper()
	raw, _ := json.Marshal(n)
	return tr.render(VerbUpdateProject, json.RawMessage(`{"project":"finance","max_concurrent_tasks":`+string(raw)+`}`))
}

func TestLimits798_ProjectConcurrency(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	if got := concurrency798(tr, "finance"); got != 1 {
		t.Fatalf("create_project renders concurrency %d, want 1", got)
	}
	for v, want := range map[int]Class{0: Refused, -1: Refused, 3: Refused} { // the cap is 2
		if c := updateConcurrency798(tr, v); c.Class != want {
			t.Errorf("max_concurrent_tasks %d: %s, want %s", v, c.Class, want)
		} else if want == Refused && v == 3 && !strings.Contains(c.Reason, "agent_admin.max_concurrent_tasks") {
			t.Errorf("the refusal does not name the key: %q", c.Reason)
		}
	}
	// Raising is Widening, with the sentence; applied, it is in force.
	c := updateConcurrency798(tr, 2)
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Sentence, "run up to 2 tasks at once (now 1).") {
		t.Errorf("sentence %q", c.Sentence)
	}
	tr.apply(c)
	if got := concurrency798(tr, "finance"); got != 2 {
		t.Fatalf("after apply concurrency is %d", got)
	}
	// Re-sending the value in force asks nothing; lowering is Inert.
	tr.mustClass(updateConcurrency798(tr, 2), Inert)
	low := updateConcurrency798(tr, 1)
	tr.mustClass(low, Inert)
	// A budget change re-renders the whole project and keeps the value.
	tr.apply(tr.render(VerbSetBudget, SetBudgetInput{Project: "finance", MonthlyUSD: 1}))
	if got := concurrency798(tr, "finance"); got != 2 {
		t.Fatalf("set_budget reset concurrency to %d", got)
	}
}

func defineRoles798(tr *tree, roles ...RoleInput) Change {
	tr.t.Helper()
	return tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: roles})
}

func hunter798(tokens, steps int) RoleInput {
	return RoleInput{Name: "hunter", Instructions: "Hunt.", Tools: []string{"file_read"}, MaxTokens: tokens, MaxToolIterations: steps}
}

func TestLimits798_RoleLimitsClassAgainstTheValueInForce(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	// Bounds.
	for _, bad := range []RoleInput{hunter798(0, -1), hunter798(-5, 0), hunter798(0, 121), hunter798(100, 0), hunter798(40000, 0)} {
		if c := defineRoles798(tr, bad); c.Class != Refused {
			t.Errorf("limits %d/%d: %s, want Refused", bad.MaxTokens, bad.MaxToolIterations, c.Class)
		}
	}
	if c := defineRoles798(tr, hunter798(0, 121)); !strings.Contains(c.Reason, "agent_admin.max_role_tool_iterations") {
		t.Errorf("the refusal does not name the key: %q", c.Reason)
	}
	// A new role at 80 steps: above the default 30, so Widening.
	c := defineRoles798(tr, hunter798(0, 80))
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Sentence, "The role hunter may use up to 80 tool steps (now 30).") {
		t.Errorf("sentence %q", c.Sentence)
	}
	tr.apply(c)
	if r := role798(tr, "finance", "hunter"); r.MaxToolIterations != 80 {
		t.Fatalf("after apply the role has %d steps", r.MaxToolIterations)
	}
	// The executor reads the rendered env var.
	sw, err := registry.ParseSwarmMarkdown([]byte(tr.files["swarms/hermes--finance.md"]), "swarms/hermes--finance.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range sw.Roles {
		if r.Name == "hunter" && r.Runtime.EnvVars["VORNIK_MAX_TOOL_ITERATIONS"] != "80" {
			t.Fatalf("rendered env %v", r.Runtime.EnvVars)
		}
	}
	// 80 → 80 asks nothing; 80 → 60 is a lowering.
	tr.mustClass(defineRoles798(tr, hunter798(0, 80)), Inert)
	tr.mustClass(defineRoles798(tr, hunter798(0, 60)), Inert)
	// Omitted means the default, named in the sentence.
	omit := defineRoles798(tr, hunter798(0, 0))
	tr.mustClass(omit, Inert)
	if !strings.Contains(omit.Sentence, "The role hunter returns to the default 30 tool steps (was 80).") {
		t.Errorf("omission sentence %q", omit.Sentence)
	}
	// Tokens: with no agent max_tokens the default is the provider's (0), so
	// any value is a raise; a pinned 4096 raised to 8000 is Widening.
	tr.mustClass(defineRoles798(tr, hunter798(4096, 80)), Widening)
	tr.apply(defineRoles798(tr, hunter798(4096, 80)))
	tr.mustClass(defineRoles798(tr, hunter798(8000, 80)), Widening)
	tr.mustClass(defineRoles798(tr, hunter798(2048, 80)), Inert)
	omitTok := defineRoles798(tr, hunter798(0, 80))
	if !strings.Contains(omitTok.Sentence, "The role hunter returns to the default output length (was 4096 tokens).") {
		t.Errorf("token omission sentence %q", omitTok.Sentence)
	}
	// With an agent max_tokens of 8192, 4096 is a lowering of a new role.
	tr2 := newTree(t, "hermes")
	tr2.agentTokens = 8192
	tr2.project("finance")
	tr2.mustClass(defineRoles798(tr2, hunter798(4096, 0)), Inert)
}

// Another verb that re-renders the swarm keeps the limits.
func TestLimits798_RoleLimitsSurviveARerender(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	tr.apply(defineRoles798(tr, hunter798(4096, 80)))
	tr.approveServer("finance", "fio", "https://api.fio.example/mcp", "balance")
	if r := role798(tr, "finance", "hunter"); r.MaxToolIterations != 80 || r.MaxTokens != 4096 {
		t.Fatalf("after add_mcp_server the role has %d steps, %d tokens", r.MaxToolIterations, r.MaxTokens)
	}
}

// review-20261118-b73e F1: omitting a pinned token limit whose default is
// higher is a raise, and the sentence names both the return and the rise.
func TestLimits798_OmittingALowPinUnderAHigherDefaultIsARaise(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.agentTokens = 8192
	tr.project("finance")
	tr.apply(defineRoles798(tr, hunter798(4096, 0)))
	c := defineRoles798(tr, hunter798(0, 0))
	tr.mustClass(c, Widening)
	for _, want := range []string{"The role hunter may write up to 8192 tokens a reply (now 4096).", "The role hunter returns to the default output length (was 4096 tokens)."} {
		if !strings.Contains(c.Sentence, want) {
			t.Errorf("sentence %q lacks %q", c.Sentence, want)
		}
	}
}
