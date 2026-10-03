package agentadmin

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// Design §18.4, incident 2026-10-02 (namespace claudecode): three
// create_project requests made against a $102 total each pinned an absolute
// $104 ceiling; approving all three left a $108 sum against a $104 ceiling.
// A request now pins what it adds and the most its sentence stated, and the
// apply decides the ceiling (CeilingAfter).

func f64(v float64) *float64 { return &v }

// input is the verb input a rendered change carries, for a re-render.
func (c Change) input(t *testing.T) json.RawMessage {
	t.Helper()
	var doc struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(c.Rendered, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Input
}

// The conditional clause and the pinned maximum come from the waiting
// requests that add spending.
func TestCeilingClause_ConditionalFromPendingAdds(t *testing.T) {
	tr := newTree(t, "hermes")
	for _, slug := range []string{"aa", "bb", "cc", "dd", "ee"} { // $10 = the ceiling
		tr.project(slug)
	}
	alone := tr.render(VerbCreateProject, CreateProjectInput{Slug: "xa", Purpose: "More"})
	tr.mustClass(alone, Widening)
	if strings.Contains(alone.Sentence, "up to") || alone.Grant.MaxTotalUSD == nil || *alone.Grant.MaxTotalUSD != 12 ||
		alone.Grant.AddsUSD == nil || *alone.Grant.AddsUSD != 2 {
		t.Fatalf("no request waits: %q %+v", alone.Sentence, alone.Grant)
	}
	tr.pending = append(tr.pending, alone)
	raise := tr.render(VerbSetBudget, SetBudgetInput{Project: "aa", MonthlyUSD: 7}) // +5
	tr.mustClass(raise, Widening)
	want := "takes your assistant's total monthly budget to $15, above the $10 you approved; approving raises the limit to $15 (up to $17 if your other waiting requests that add spending are also approved)."
	if !strings.Contains(raise.Sentence, want) || raise.Grant.MaxTotalUSD == nil || *raise.Grant.MaxTotalUSD != 17 ||
		raise.Grant.AddsUSD == nil || *raise.Grant.AddsUSD != 5 || raise.Grant.CeilingUSD != nil {
		t.Fatalf("sentence %q grant %+v", raise.Sentence, raise.Grant)
	}
}

// A budget raise within the ceiling states no total: it pins what it adds and
// no maximum, so the apply allows it only within the ceiling then in force.
func TestCeilingClause_RaiseWithinCeilingPinsNoMaximum(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("aa")
	c := tr.render(VerbSetBudget, SetBudgetInput{Project: "aa", MonthlyUSD: 5})
	tr.mustClass(c, Widening)
	if c.Grant.AddsUSD == nil || *c.Grant.AddsUSD != 3 || c.Grant.MaxTotalUSD != nil || strings.Contains(c.Sentence, "total") {
		t.Fatalf("grant %+v sentence %q", c.Grant, c.Sentence)
	}
}

func TestCeilingAfter(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		g                      Grant
		ceiling, before, after float64
		want                   float64
		ok                     bool
	}{
		{"does not raise the sum", Grant{}, 10, 12, 12, 10, true},
		{"lowers the sum", Grant{}, 10, 12, 11, 10, true},
		{"within the ceiling", Grant{AddsUSD: f64(2)}, 10, 6, 8, 10, true},
		{"raises to the sum", Grant{AddsUSD: f64(2), MaxTotalUSD: f64(14)}, 10, 10, 12, 12, true},
		{"exactly the stated maximum", Grant{AddsUSD: f64(2), MaxTotalUSD: f64(14)}, 12, 12, 14, 14, true},
		{"exceeds the stated maximum", Grant{AddsUSD: f64(2), MaxTotalUSD: f64(12)}, 12, 12, 14, 12, false},
		{"no stated maximum, over the ceiling", Grant{AddsUSD: f64(3)}, 10, 9, 12, 10, false},
		{"a ceiling already above never lowers", Grant{AddsUSD: f64(2), MaxTotalUSD: f64(12)}, 20, 10, 12, 20, true},
		{"legacy ceiling_usd reads as the maximum", Grant{CeilingUSD: f64(12)}, 10, 10, 12, 12, true},
		{"legacy ceiling_usd exceeded", Grant{CeilingUSD: f64(12)}, 12, 12, 14, 12, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CeilingAfter(tc.g, tc.ceiling, tc.before, tc.after)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("CeilingAfter = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// BudgetTotalAfter reads the caps a change's project files carry.
func TestBudgetTotalAfter(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("aa")
	tr.project("bb")
	st := tr.state()
	create := tr.render(VerbCreateProject, CreateProjectInput{Slug: "cc", Purpose: "More"})
	raise := tr.render(VerbSetBudget, SetBudgetInput{Project: "aa", MonthlyUSD: 7.5})
	remove := tr.render(VerbRemove, RemoveInput{Kind: "project", ID: "bb"})
	for _, tc := range []struct {
		c    Change
		want float64
	}{{create, 6}, {raise, 9.5}, {remove, 2}, {Change{}, 4}} {
		got, err := st.BudgetTotalAfter(tc.c.Ops)
		if err != nil || got != tc.want {
			t.Fatalf("%s: %v, %v; want %v", tc.c.Verb, got, err, tc.want)
		}
	}
	if _, err := st.BudgetTotalAfter([]FileOp{{Op: OpReplace, Path: "projects/hermes--aa.yaml", Content: "budget: ["}}); err == nil {
		t.Fatal("an unparseable project file was summed")
	}
}

// approveInTree is the widening_change effect over the tree: refused when the
// stated maximum is exceeded (the caller re-renders), else applied.
func approveInTree(t *testing.T, tr *tree, c Change) bool {
	t.Helper()
	st := tr.state()
	after, err := st.BudgetTotalAfter(c.Ops)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := CeilingAfter(c.Grant, st.CeilingUSD, st.BudgetTotal(), after); !ok {
		return false
	}
	tr.apply(c)
	return true
}

// Property (§18.4): after any sequence of approvals and rejections of
// spending requests, the namespace sum never exceeds the ceiling, a request
// refused at apply is re-rendered against the requests then waiting, and
// re-rendering converges (F4).
func TestCeiling_PropertyApprovalsAndRejections(t *testing.T) {
	const seeds = 60
	reRendered, applied := 0, 0
	for seed := int64(1); seed <= seeds; seed++ {
		rng := rand.New(rand.NewSource(seed))
		tr := newTree(t, "hermes")
		slugs := []string{"aa", "bb", "cc", "dd", "ee"}
		for _, s := range slugs {
			tr.project(s)
		}
		n := 2 + rng.Intn(4)
		for i := 0; i < n; i++ {
			var c Change
			if rng.Intn(2) == 0 {
				c = tr.render(VerbCreateProject, CreateProjectInput{Slug: fmt.Sprintf("n%d", i), Purpose: "More"})
			} else {
				c = tr.render(VerbSetBudget, SetBudgetInput{Project: slugs[i], MonthlyUSD: float64(3 + rng.Intn(9))})
			}
			tr.mustClass(c, Widening)
			tr.pending = append(tr.pending, c)
		}
		for steps := 0; len(tr.pending) > 0; steps++ {
			if steps > 50 {
				t.Fatalf("seed %d: re-rendering did not converge", seed)
			}
			k := rng.Intn(len(tr.pending))
			c := tr.pending[k]
			tr.pending = append(tr.pending[:k:k], tr.pending[k+1:]...)
			if rng.Intn(4) == 0 {
				continue // rejected
			}
			if !approveInTree(t, tr, c) {
				again := tr.render(c.Verb, c.input(t))
				tr.mustClass(again, Widening)
				tr.pending = append(tr.pending, again)
				reRendered++
				continue
			}
			applied++
			if st := tr.state(); st.BudgetTotal() > st.CeilingUSD+1e-9 {
				t.Fatalf("seed %d step %d: sum $%v exceeds the ceiling $%v", seed, steps, st.BudgetTotal(), st.CeilingUSD)
			}
		}
	}
	// The denominator: a run that never refused proved nothing about refusal.
	t.Logf("%d seeds: %d applied, %d refused and re-rendered", seeds, applied, reRendered)
	if applied == 0 || reRendered == 0 {
		t.Fatalf("the sequences exercised %d applies and %d re-renders; both must occur", applied, reRendered)
	}
}

// Two raises with DIFFERENT adds (+2, then +5 made while the first waits),
// approved in each order (review d94f F3): the ceiling ends equal to the sum,
// and only "b then a" needs a re-render (a stated $12; the sum would be $17).
func TestCeiling_DifferentAddsInEachOrder(t *testing.T) {
	for _, bFirst := range []bool{false, true} {
		tr := newTree(t, "hermes")
		for _, s := range []string{"aa", "bb", "cc", "dd", "ee"} {
			tr.project(s)
		}
		a := tr.render(VerbSetBudget, SetBudgetInput{Project: "aa", MonthlyUSD: 4})
		tr.pending = []Change{a}
		b := tr.render(VerbSetBudget, SetBudgetInput{Project: "bb", MonthlyUSD: 7})
		tr.pending = nil
		order := []Change{a, b}
		if bFirst {
			order = []Change{b, a}
		}
		for i, c := range order {
			if approveInTree(t, tr, c) {
				continue
			}
			if !bFirst || i != 1 {
				t.Fatalf("b_first=%v: request %d refused", bFirst, i)
			}
			if !approveInTree(t, tr, tr.render(c.Verb, c.input(t))) {
				t.Fatalf("b_first=%v: the re-rendered request was refused", bFirst)
			}
		}
		if st := tr.state(); st.BudgetTotal() != 17 || st.CeilingUSD != 17 {
			t.Fatalf("b_first=%v: sum $%v ceiling $%v, want $17", bFirst, st.BudgetTotal(), st.CeilingUSD)
		}
	}
}
