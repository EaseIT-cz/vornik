package agentadmin

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// GitHub #172 (2026-10-13): a refused agent-admin verb left no row, so an
// assistant probing its boundaries was invisible to the operator. The row
// records a rule from a closed set, never the reason text (which quotes the
// agent's input). Design §23.2. Each case is a real refusal through Render.
func TestRefusalRule_RealRefusalsAreClassified(t *testing.T) {
	egressBad := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"x":{"type":"string"}}}`)
	cases := []struct {
		name string
		want string
		run  func(tr *tree) Change
	}{
		{"http URL", RuleURLNotHTTPS, func(tr *tree) Change {
			a := fioAPI()
			a.BaseURL = "http://bank.example/v1"
			return tr.render(VerbAddAPI, a)
		}},
		{"malformed slug", RuleInvalidInput, func(tr *tree) Change {
			return tr.render(VerbCreateProject, CreateProjectInput{Slug: "Bad Slug", Purpose: "x"})
		}},
		{"unknown field", RuleInvalidInput, func(tr *tree) Change {
			c, err := tr.r.Render(tr.state(), VerbCreateProject, json.RawMessage(`{"slug":"ok","purpose":"x","bogus":1}`))
			if err != nil {
				t.Fatal(err)
			}
			return c
		}},
		{"unknown project", RuleNotFound, func(tr *tree) Change {
			return tr.render(VerbSetBudget, SetBudgetInput{Project: "nope", MonthlyUSD: 1})
		}},
		{"existing project", RuleAlreadyExists, func(tr *tree) Change {
			return tr.render(VerbCreateProject, CreateProjectInput{Slug: "finance", Purpose: "again"})
		}},
		{"budget above the cap", RuleLimitExceeded, func(tr *tree) Change {
			return tr.render(VerbSetBudget, SetBudgetInput{Project: "finance", MonthlyUSD: maxBudgetUSD + 1})
		}},
		{"bad egress", RuleEgressSchema, func(tr *tree) Change {
			return tr.render(VerbDefineWorkflow, DefineWorkflowInput{Project: "finance", Slug: "spend", Purpose: "p",
				Steps:  []StepInput{{Name: "s", Role: "worker", Instructions: "x"}},
				Inputs: []byte(`{"type":"object","additionalProperties":false,"properties":{}}`), Egress: egressBad})
		}},
		{"pending change", RulePendingChange, func(tr *tree) Change {
			first := tr.render(VerbAddAPI, fioAPI())
			tr.mustClass(first, Widening)
			tr.pending = append(tr.pending, first)
			return tr.render(VerbAddAPI, fioAPI())
		}},
		{"another namespace's project", RuleOutOfNamespace, func(tr *tree) Change {
			return tr.render(VerbSetBudget, SetBudgetInput{Project: "codex--finance", MonthlyUSD: 1})
		}},
		{"another namespace's project to remove", RuleOutOfNamespace, func(tr *tree) Change {
			return tr.render(VerbRemove, RemoveInput{Kind: "project", ID: "codex--finance"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := newTree(t, "hermes")
			tr.project("finance")
			ch := c.run(tr)
			if ch.Class != Refused {
				t.Fatalf("not refused: %+v", ch)
			}
			if ch.Rule != c.want {
				t.Fatalf("rule %q, want %q (reason %q)", ch.Rule, c.want, ch.Reason)
			}
		})
	}
}

// A workflow removal names "<project>--<workflow>"; that is not another
// namespace's id and must not be classified as one.
func TestRefusalRule_WorkflowRemovalIsNotOutOfNamespace(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	ch := tr.render(VerbRemove, RemoveInput{Kind: "workflow", ID: "finance--nope"})
	if ch.Class != Refused || ch.Rule != RuleNotFound {
		t.Fatalf("rule %q (reason %q), want %q", ch.Rule, ch.Reason, RuleNotFound)
	}
}

// The pattern table's order: a reason matching two patterns takes the
// earlier rule; every rule the table returns is in the closed set; a reason
// no pattern matches is "other".
func TestRefusalRule_TableOrderAndClosedSet(t *testing.T) {
	for reason, want := range map[string]string{
		"x refused: hermes--a is part of a change waiting for approval; there is no project": RulePendingChange,
		"x refused: the egress schema is over 8192 bytes":                                    RuleEgressSchema,
		"x refused: the slug \"a\" is too long for this namespace; use at most 3 characters": RuleLimitExceeded,
		"x refused: name is 40 characters; the limit is 32":                                  RuleLimitExceeded,
		"x refused: http://a is not https (plain http is allowed only to this machine)":      RuleURLNotHTTPS,
		"x refused: hermes--a already exists":                                                RuleAlreadyExists,
		"x refused: monthly_usd must be above zero; a budget cannot be removed or unlimited": RuleInvalidInput,
		"x refused: something nobody anticipated":                                            RuleOther,
	} {
		if got := refusalRule(reason); got != want {
			t.Errorf("refusalRule(%q) = %q, want %q", reason, got, want)
		}
	}
	for _, p := range refusalPatterns {
		if !slices.Contains(RefusalRules, p.rule) {
			t.Errorf("pattern %q returns %q, which is not in RefusalRules", p.text, p.rule)
		}
	}
	for _, r := range RefusalRules {
		if r == "" || strings.ToLower(r) != r || strings.ContainsAny(r, " -") {
			t.Errorf("rule %q is not a snake_case label", r)
		}
	}
	if !slices.Contains(RefusalRules, RuleOther) {
		t.Error("RefusalRules lacks other")
	}
}
