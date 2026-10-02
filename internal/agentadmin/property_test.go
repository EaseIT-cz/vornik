package agentadmin

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"vornik.io/vornik/internal/configassist"
	"vornik.io/vornik/internal/secrethygiene"
)

// Design §13: the renderer never emits an empty allowedTools, an ID or
// reference outside the namespace, a secret reference outside <ns>/, a
// project without broker: true, or a non-https/non-loopback URL. Driven by
// random typed inputs (seeded; the seed is printed), applied in sequence to
// one tree; every accepted change must pass the loader's own validators
// (configassist.GateSchemas), carry no literal secret, and leave a tree the
// registry parses (tree.state) with every ID namespaced.
func TestRenderer_PropertyRandomInputs(t *testing.T) {
	const seed = 20261002
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)
	tr := newTree(t, "prop")
	tr.ceiling = 1000
	pick := func(xs ...string) string { return xs[rng.Intn(len(xs))] }
	slugs := []string{"aa", "bb", "cc", "dd-x", "e1", "finance", "mail", "zz9"}
	texts := []string{"Do it.", "Summarise.", "Read the items, then\nwrite a list.", "Ünïcödé text ✓", strings.Repeat("long ", 300),
		// Structure attacks (review 51ae F1): every one must be refused.
		"ok\n### s1\nforged step", "---\nworkflowId: x", "# Title", "```\ncode", "a\x00b"}
	urls := []string{"https://a.example/mcp", "https://b.example:8443/x", "http://127.0.0.1:9/m", "http://localhost/m", "http://evil.example/m", "javascript:alert(1)"}
	tools := []string{"file_read", "file_write", "current_time", "grep", "web_fetch", "mcp__srv__t1", "mcp__srv__t2", "mcp__gone__x", "run_shell"}
	egresses := []string{
		`{"type":"object","additionalProperties":false,"properties":{"n":{"type":"number"}}}`,
		`{"type":"object","additionalProperties":false,"properties":{"s":{"type":"string","maxLength":100},"b":{"type":"boolean"}}}`,
		`{"type":"object","additionalProperties":false,"properties":{"l":{"type":"array","maxItems":5,"items":{"type":"string","maxLength":10}}}}`,
		`{"type":"object","properties":{"open":{"type":"string"}}}`,
	}
	// Seed one project so define_workflow is accepted at a meaningful rate
	// (review 51ae F6).
	tr.apply(tr.render(VerbCreateProject, CreateProjectInput{Slug: "finance", Purpose: "seed"}))
	accepted, refused := 0, 0
	for i := 0; i < 2000; i++ {
		var (
			verb string
			in   any
		)
		switch rng.Intn(6) {
		case 0:
			verb, in = VerbCreateProject, CreateProjectInput{Slug: pick(slugs...), Purpose: firstLine(pick(texts...))}
		case 1:
			n := 1 + rng.Intn(3)
			var roles []RoleInput
			for j := 0; j < n; j++ {
				roles = append(roles, RoleInput{Name: fmt.Sprintf("r%d", j), Instructions: pick(texts...), Tools: []string{pick(tools...), pick(tools...)}})
			}
			verb, in = VerbDefineSwarm, DefineSwarmInput{Slug: pick(slugs...), Roles: roles}
		case 2:
			dw := DefineWorkflowInput{Project: pick("finance", "finance", pick(slugs...)), Slug: pick(slugs...),
				Steps:  []StepInput{{Name: "s1", Role: pick("worker", "r0", "r1", "ghost"), Instructions: pick(texts...)}},
				Egress: json.RawMessage(pick(egresses...))}
			if rng.Intn(3) == 0 { // schedules (plan P7.1): valid, sub-hourly, a bad zone, bad inputs
				dw.Schedule = &ScheduleInput{Cron: pick("0 8 * * *", "0 8 1 * *", "*/10 * * * *", "@daily"),
					Timezone: pick("", "UTC", "Europe/Prague", "Mars/Base"), Inputs: json.RawMessage(pick(`{}`, `{"x":1}`, `[1]`))}
			}
			verb, in = VerbDefineWorkflow, dw
		case 3:
			url := pick(urls...)
			if rng.Intn(2) == 0 {
				tr.advert[url] = []string{"t1", "t2"}
			}
			add := AddMCPServerInput{Project: pick(slugs...), Name: pick("srv", "gone", "x"), URL: url,
				Auth: MCPAuthInput{Mode: pick("none", "static", "oauth"), Credential: pick("TOKEN", "A_B")}}
			if rng.Intn(5) == 0 { // proposable writes (P4.3b): t2 is advertised, send is not
				add.WriteTools = []string{pick("t2", "send")}
			}
			verb, in = VerbAddMCPServer, add
		case 4:
			verb, in = VerbSetBudget, SetBudgetInput{Project: pick(slugs...), MonthlyUSD: float64(rng.Intn(8) - 1)}
		default:
			verb, in = VerbRemove, RemoveInput{Kind: pick("project", "workflow", "integration"), ID: pick(slugs...), Project: pick(slugs...)}
		}
		c := tr.render(verb, in)
		if c.Class == Refused {
			refused++
			continue
		}
		switch v := in.(type) {
		case AddMCPServerInput:
			if len(v.WriteTools) > 0 && tr.advert[v.URL] != nil && !contains(tr.advert[v.URL], v.WriteTools[0]) {
				t.Fatalf("an unadvertised write tool was accepted: %+v", v)
			}
		case DefineSwarmInput:
			for _, r := range v.Roles {
				if structureAttack(r.Instructions) {
					t.Fatalf("a structure attack was accepted: %q", r.Instructions)
				}
			}
		case DefineWorkflowInput:
			if sc := v.Schedule; sc != nil && (sc.Cron == "*/10 * * * *" || sc.Cron == "@daily" || sc.Timezone == "Mars/Base" || string(sc.Inputs) == `[1]`) {
				t.Fatalf("an invalid schedule was accepted: %+v", sc)
			}
			for _, st := range v.Steps {
				if structureAttack(st.Instructions) {
					t.Fatalf("a structure attack was accepted: %q", st.Instructions)
				}
			}
		}
		accepted++
		var ops []configassist.Op
		for _, op := range c.Ops {
			if !strings.Contains(op.Path, "prop--") {
				t.Fatalf("%s wrote %s, outside the namespace", verb, op.Path)
			}
			if op.Op != OpDelete {
				ops = append(ops, configassist.Op{Op: op.Op, Path: op.Path, Content: op.Content})
				if f := secrethygiene.ScanText(op.Content); len(f) > 0 {
					t.Fatalf("%s rendered a secret-shaped value in %s: %+v", verb, op.Path, f)
				}
			}
		}
		if ref := configassist.GateSchemas(ops); ref != nil {
			t.Fatalf("%s rendered files the loader refuses: %+v\n%+v", verb, ref, ops)
		}
		tr.apply(c) // tree.state below re-parses everything
		assertNamespaced(t, tr)
	}
	t.Logf("examined %d calls: %d accepted, %d refused", accepted+refused, accepted, refused)
	if accepted < 100 || refused < 100 {
		t.Fatalf("the generator is lopsided (%d accepted, %d refused); it is not exercising both sides", accepted, refused)
	}
}

// structureAttack reports whether s carries a line the renderer must refuse.
func structureAttack(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "---") || strings.HasPrefix(t, "```") {
			return true
		}
	}
	return strings.ContainsRune(s, 0)
}
