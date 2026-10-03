package agentadmin

import (
	"fmt"
	"math"
	"strings"

	"gopkg.in/yaml.v3"
)

// The namespace ceiling across concurrent requests (design §18.4, incident
// 2026-10-02: three requests made against one total each pinned an absolute
// ceiling; the last approval won and the sum exceeded it). A request pins
// what it adds and the most its sentence stated; the apply, under the
// service's lock, decides the ceiling with CeilingAfter.

// usdEpsilon absorbs float noise in dollar comparisons.
const usdEpsilon = 1e-9

// FormatUSD formats dollars as the approval sentences do ("12", "7.5").
func FormatUSD(v float64) string { return formatUSD(v) }

// BudgetTotal sums the namespace's project caps.
func (s *State) BudgetTotal() float64 { return s.budgetTotal() }

// BudgetTotalAfter is the namespace sum once ops apply: each project file an
// op creates or replaces counts with the cap it carries, a deleted one counts
// nothing. It reads the change's own bytes, so it holds for a request filed
// before AddsUSD existed.
func (s *State) BudgetTotalAfter(ops []FileOp) (float64, error) {
	caps := make(map[string]float64, len(s.Projects))
	for id, p := range s.Projects {
		caps[id] = p.MonthlyUSD
	}
	for _, op := range ops {
		file, ok := strings.CutPrefix(op.Path, "projects/")
		if !ok || !strings.HasSuffix(file, ".yaml") {
			continue
		}
		id := strings.TrimSuffix(file, ".yaml")
		if op.Op == OpDelete {
			delete(caps, id)
			continue
		}
		var doc struct {
			Budget struct {
				MonthlyHardUSD float64 `yaml:"monthly_hard_usd"`
			} `yaml:"budget"`
		}
		if err := yaml.Unmarshal([]byte(op.Content), &doc); err != nil {
			return 0, fmt.Errorf("%s: %w", op.Path, err)
		}
		caps[id] = doc.Budget.MonthlyHardUSD
	}
	t := 0.0
	for _, v := range caps {
		t += v
	}
	return t, nil
}

// CeilingAfter decides an approved change's apply from the ceiling in force
// and the namespace sum before and after it. A change that does not raise
// the sum always applies and leaves the ceiling alone. One that does applies
// only if the sum after it is at most max(ceiling, the most its sentence
// stated); the ceiling then becomes max(ceiling, sum after): never lowered,
// never above a total the user approved. ok false means refuse: nothing
// applies, and the request is re-rendered with the true figures.
//
// A request filed before §18.4 carries only the absolute CeilingUSD it
// pinned; that is the most its sentence stated, so it is read as the maximum.
func CeilingAfter(g Grant, ceiling, before, after float64) (float64, bool) {
	if after <= before+usdEpsilon {
		return ceiling, true
	}
	allowed := ceiling
	switch {
	case g.MaxTotalUSD != nil:
		allowed = math.Max(allowed, *g.MaxTotalUSD)
	case g.CeilingUSD != nil:
		allowed = math.Max(allowed, *g.CeilingUSD)
	}
	if after > allowed+usdEpsilon {
		return ceiling, false
	}
	return math.Max(ceiling, after), true
}

// MaxAllowedTotal is the highest namespace sum a change's approval covers
// given the ceiling in force: what CeilingAfter checks against.
func MaxAllowedTotal(g Grant, ceiling float64) float64 {
	switch {
	case g.MaxTotalUSD != nil:
		return math.Max(ceiling, *g.MaxTotalUSD)
	case g.CeilingUSD != nil:
		return math.Max(ceiling, *g.CeilingUSD)
	}
	return ceiling
}

// spendingTerms states a change that adds adds to the namespace sum and
// fills what its grant pins. It is the one place the ceiling clause is
// rendered: the first render and a re-render after a refused apply both come
// here. It reports whether the change takes the sum above the ceiling (the
// change is then widening for that reason).
//
// The primary total is the sum with this change alone; the conditional one
// adds every OTHER waiting request that adds spending (st.PendingAddsUSD), a
// snapshot labelled as such. The pinned maximum is the conditional total
// when one is stated, else the primary. A change within the ceiling states
// no total and pins no maximum: the apply allows it within the ceiling then
// in force.
func spendingTerms(st *State, adds float64, g *Grant) (clause string, raises bool) {
	a := adds
	g.AddsUSD = &a
	primary := st.budgetTotal() + adds
	if primary <= st.CeilingUSD+usdEpsilon {
		return "", false
	}
	maxTotal := primary
	clause = fmt.Sprintf(" That takes your assistant's total monthly budget to $%s, above the $%s you approved; approving raises the limit to $%s",
		formatUSD(primary), formatUSD(st.CeilingUSD), formatUSD(primary))
	if st.PendingAddsUSD > usdEpsilon {
		maxTotal = primary + st.PendingAddsUSD
		clause += fmt.Sprintf(" (up to $%s if your other waiting requests that add spending are also approved)", formatUSD(maxTotal))
	}
	g.MaxTotalUSD = &maxTotal
	return clause + ".", true
}
