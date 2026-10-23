package executor

// GitHub #441 (security audit 2026-10-17): "the taint-lineage D7 latch
// suppresses a High-severity review when the lineage also has an Unknown
// step". Found at: review (code audit). These tests sit at the level the
// review decision is taken: the executor's forge review gate
// (TaintReviewer.ReviewForgeWrite → resolveTaintDecision), reading rows that
// were written through the executor's own stamp (Classify →
// taintStampFromStep) into a real SQLite execution_step_outcomes table.
// Design: taint-lineage-tracking-design.md §15 (Amendment 2026-10-21, #441).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/taintlineage"
)

// sev441 is a bit set of the severity classes a step's calls touch.
type sev441 uint8

const (
	sevLow441 sev441 = 1 << iota
	sevHigh441
	sevUnknown441
)

func (s sev441) String() string {
	if s == 0 {
		return "none"
	}
	var p []string
	if s&sevLow441 != 0 {
		p = append(p, "low")
	}
	if s&sevHigh441 != 0 {
		p = append(p, "high")
	}
	if s&sevUnknown441 != 0 {
		p = append(p, "unknown")
	}
	return strings.Join(p, "+")
}

// calls441 builds the audited tool calls for a step touching the given
// classes. A distinct tag keeps sources of different steps distinct.
func calls441(s sev441, tag string) []taintlineage.ToolCall {
	c := []taintlineage.ToolCall{{Tool: "file_read", Input: `{}`}} // None, always
	if s&sevLow441 != 0 {
		c = append(c, taintlineage.ToolCall{Tool: "memory_search", Input: `{"query":"q-` + tag + `"}`})
	}
	if s&sevHigh441 != 0 {
		c = append(c, taintlineage.ToolCall{Tool: "web_fetch", Input: `{"url":"https://x.example/` + tag + `"}`})
	}
	if s&sevUnknown441 != 0 {
		c = append(c, taintlineage.ToolCall{Tool: "mystery_tool_" + tag, Input: `{}`})
	}
	return c
}

type gate441 struct {
	t    *testing.T
	repo *sqlite.ExecutionStepOutcomeRepository
	n    int
}

func newGate441(t *testing.T) *gate441 {
	t.Helper()
	db := sqlitetest.Memory(t)
	return &gate441{t: t, repo: sqlite.NewExecutionStepOutcomeRepository(db.DB)}
}

// recordStamp writes one agent-step row carrying the given taint stamp.
func (g *gate441) recordStamp(taskID string, stamp taintStamp) {
	g.t.Helper()
	g.n++
	row := &persistence.ExecutionStepOutcome{
		ID: fmt.Sprintf("o-%d", g.n), ProjectID: "p", TaskID: taskID,
		ExecutionID: "e-" + taskID, StepID: fmt.Sprintf("s%d", g.n), Role: "worker", Outcome: "success",
		UntrustedContentUsed: stamp.UntrustedContentUsed,
		UntrustedSources:     stamp.UntrustedSources,
		RequiresReview:       stamp.RequiresReview,
	}
	if err := g.repo.Record(context.Background(), row); err != nil {
		g.t.Fatalf("record outcome: %v", err)
	}
}

// recordCalls classifies and stamps a step exactly as the executor does
// (persistToolAuditFromResult → taintStampFromStep) and returns the stamp.
func (g *gate441) recordCalls(taskID string, calls []taintlineage.ToolCall) taintStamp {
	g.t.Helper()
	stamp := (*Executor)(nil).taintStampFromStep(taintlineage.Classify(calls))
	g.recordStamp(taskID, stamp)
	return stamp
}

func (g *gate441) reviewer(tasks map[string]*persistence.Task, latches []string, mode taintlineage.Mode) *TaintReviewer {
	return NewTaintReviewer(g.repo, &taintLister{byID: tasks}, &taintMsgRepo{latches: latches},
		func(string) taintlineage.Mode { return mode }, nil)
}

// parkHash runs the enforce gate with no latch and returns the source-set
// hash the park checkpoint would carry — the value an admin "allow" records
// as the D7 latch.
func (g *gate441) parkHash(taskID string) string {
	g.t.Helper()
	tr := g.reviewer(map[string]*persistence.Task{taskID: {ID: taskID}}, nil, taintlineage.ModeEnforce)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", taskID)
	if err != nil {
		g.t.Fatalf("park: %v", err)
	}
	if sig == nil {
		return ""
	}
	return sig.SourceSetHash
}

// TestTaintGate441_SeverityPrecedenceTable runs every per-step severity
// combination (one or two steps per lineage) through the executor gate in
// advisory and enforce, with no latch, a latch for exactly this set, and a
// latch for a reviewed subset (the Unknown/Low half only), on a complete and
// an incomplete walk. The rule under test: High and Unknown are independent
// classes, neither is decided by the other, and the latch suppresses only a
// set an admin was shown in full.
func TestTaintGate441_SeverityPrecedenceTable(t *testing.T) {
	g := newGate441(t)
	all := []sev441{0, sevLow441, sevHigh441, sevUnknown441,
		sevLow441 | sevHigh441, sevLow441 | sevUnknown441, sevHigh441 | sevUnknown441,
		sevLow441 | sevHigh441 | sevUnknown441}

	var lineages [][]sev441
	for _, a := range all {
		lineages = append(lineages, []sev441{a})
		for _, b := range all {
			lineages = append(lineages, []sev441{a, b})
		}
	}

	for li, steps := range lineages {
		names := make([]string, len(steps))
		anyHigh, anyUnknown := false, false
		for i, s := range steps {
			names[i] = s.String()
			anyHigh = anyHigh || s&sevHigh441 != 0
			anyUnknown = anyUnknown || s&sevUnknown441 != 0
		}
		task := fmt.Sprintf("t%d", li)
		for i, s := range steps {
			stamp := g.recordCalls(task, calls441(s, fmt.Sprintf("%d-%d", li, i)))
			// The STORED flag: any High call, whatever else the step called.
			if got, want := stamp.RequiresReview, s&sevHigh441 != 0; got != want {
				t.Errorf("[%s] step %d stored requires_review=%v, want %v (High must not be hidden by Unknown)",
					strings.Join(names, ","), i, got, want)
			}
		}
		exact := g.parkHash(task)

		// The reviewed SUBSET: the same lineage minus its High calls, on its
		// own task, parked and allowed. Its latch must never clear this set.
		subTask := task + "-sub"
		for i, s := range steps {
			sub := s &^ sevHigh441
			g.recordCalls(subTask, calls441(sub, fmt.Sprintf("%d-%d", li, i)))
		}
		subset := g.parkHash(subTask)
		// A lineage with a High call: the latch for its High-free half is a
		// DIFFERENT key, so it can never clear the High review (the issue's
		// scenario). Without High the half is the whole set and may match.
		if anyHigh && subset != "" && subset == exact {
			t.Errorf("[%s] latch for the High-free subset equals the full set's key", strings.Join(names, ","))
		}

		contentReview := anyHigh || anyUnknown
		for _, mode := range []taintlineage.Mode{taintlineage.ModeAdvisory, taintlineage.ModeEnforce} {
			for _, latch := range []string{"none", "exact", "subset"} {
				for _, complete := range []bool{true, false} {
					name := fmt.Sprintf("%s/%s/latch=%s/complete=%v", strings.Join(names, ","), mode, latch, complete)
					var latches []string
					switch latch {
					case "exact":
						latches = []string{exact}
					case "subset":
						latches = []string{subset}
					}
					tasks := map[string]*persistence.Task{task: {ID: task}}
					if !complete {
						tasks[task] = &persistence.Task{ID: task, ParentTaskID: strptrExec("missing-parent")}
					}
					tr := g.reviewer(tasks, latches, mode)

					dec, err := resolveTaintDecision(context.Background(), mode, task, tr.taskLister, tr.outcomeRepo, tr.msgRepo)
					if err != nil {
						t.Fatalf("[%s] resolve: %v", name, err)
					}
					wantReview := anyHigh
					if mode == taintlineage.ModeEnforce {
						wantReview = contentReview || !complete
					}
					if dec.RequiresReview != wantReview {
						t.Errorf("[%s] decision RequiresReview=%v, want %v", name, dec.RequiresReview, wantReview)
					}

					sig, err := tr.ReviewForgeWrite(context.Background(), "p", task)
					if err != nil {
						t.Fatalf("[%s] review: %v", name, err)
					}
					wantPark := false
					if mode == taintlineage.ModeEnforce {
						latched := len(latches) == 1 && latches[0] != "" && latches[0] == exact
						wantPark = !complete || (contentReview && !latched)
					}
					if (sig != nil) != wantPark {
						t.Errorf("[%s] park=%v, want %v", name, sig != nil, wantPark)
					}
				}
			}
		}
	}
}

// G1: a source reviewed while the classifier graded it Unknown must not clear
// the same source once it is graded High (a classifier promotion between the
// park and the re-run; web_search/web_scrape/http_get were promoted this way).
func TestTaintGate441_LatchIsBoundToReviewedSeverity(t *testing.T) {
	g := newGate441(t)
	stampFor := func(sev taintlineage.Severity) taintStamp {
		src := []taintlineage.Source{{Tool: "web_scrape", Ref: "https://x.example/p", Severity: sev}}
		st := taintlineage.StepTaint{Used: true, MaxSeverity: sev, RequiresReview: sev == taintlineage.SeverityHigh,
			Sources: src, FullHash: taintlineage.HashSources(src)}
		return (*Executor)(nil).taintStampFromStep(st)
	}
	g.recordStamp("t", stampFor(taintlineage.SeverityUnknown)) // the parked run, old classifier
	latch := g.parkHash("t")
	if latch == "" {
		t.Fatal("the Unknown run must park with a hash")
	}
	g.recordStamp("t", stampFor(taintlineage.SeverityHigh)) // the re-run, new classifier
	tr := g.reviewer(map[string]*persistence.Task{"t": {ID: "t"}}, []string{latch}, taintlineage.ModeEnforce)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig == nil {
		t.Fatalf("a latch granted on an Unknown-graded set must not clear the same set graded High; sig=%v err=%v", sig, err)
	}
}

// G2/G3: a row the taint index returns but whose severity cannot be
// established (undecodable blob, no sources, out-of-range severity) must fail
// closed in enforce, not pass as untainted-enough.
func TestTaintGate441_UngradableTaintedRowFailsClosed(t *testing.T) {
	cases := map[string][]byte{
		"undecodable":    []byte(`{"sources":"not-a-list"`),
		"empty-list":     []byte(`[]`),
		"empty-object":   []byte(`{"sources":[]}`),
		"severity-7":     []byte(`[{"tool":"x","ref":"x","severity":7}]`),
		"severity-minus": []byte(`[{"tool":"x","ref":"x","severity":-1}]`),
		"severity-none":  []byte(`[{"tool":"x","ref":"x","severity":0}]`),
	}
	for name, blob := range cases {
		t.Run(name, func(t *testing.T) {
			g := newGate441(t)
			g.recordStamp("t", taintStamp{UntrustedContentUsed: true, UntrustedSources: blob})
			tr := g.reviewer(map[string]*persistence.Task{"t": {ID: "t"}}, nil, taintlineage.ModeEnforce)
			sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
			if err != nil || sig == nil {
				t.Fatalf("enforce must park a tainted row whose severity cannot be established; sig=%v err=%v", sig, err)
			}
		})
	}
}

// G4: the per-step source cap must not hide the class that decides. 16
// distinct Low sources then one Unknown call: the stored 16 must not
// reconstruct as Low-only.
func TestTaintGate441_SourceCapDoesNotHideUnknown(t *testing.T) {
	g := newGate441(t)
	var calls []taintlineage.ToolCall
	for i := 0; i < taintlineage.MaxSources; i++ {
		calls = append(calls, taintlineage.ToolCall{Tool: "memory_search", Input: fmt.Sprintf(`{"query":"q%02d"}`, i)})
	}
	calls = append(calls, taintlineage.ToolCall{Tool: "mystery_tool", Input: `{}`})
	g.recordCalls("t", calls)
	tr := g.reviewer(map[string]*persistence.Task{"t": {ID: "t"}}, nil, taintlineage.ModeEnforce)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig == nil {
		t.Fatalf("an Unknown call dropped by the per-step cap must still park in enforce; sig=%v err=%v", sig, err)
	}
}

// G4, legacy rows: a blob written before #441 carries no max_severity; when
// its cap dropped sources, their severity is unknown, so it fails closed.
func TestTaintGate441_LegacyCappedBlobFailsClosed(t *testing.T) {
	g := newGate441(t)
	var src []taintlineage.Source
	for i := 0; i < taintlineage.MaxSources; i++ {
		src = append(src, taintlineage.Source{Tool: "memory_search", Ref: fmt.Sprintf("q%02d", i), Severity: taintlineage.SeverityLow})
	}
	blob, _ := json.Marshal(map[string]any{"full_hash": "legacyhash", "dropped": 1, "sources": src})
	g.recordStamp("t", taintStamp{UntrustedContentUsed: true, UntrustedSources: blob})
	tr := g.reviewer(map[string]*persistence.Task{"t": {ID: "t"}}, nil, taintlineage.ModeEnforce)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig == nil {
		t.Fatalf("a legacy capped blob with dropped sources must park in enforce; sig=%v err=%v", sig, err)
	}
}

// G5: with more than MaxSources lineage sources, the checkpoint shown to the
// reviewer must include the High source that caused the park.
func TestTaintGate441_CheckpointShowsHighSourceFirst(t *testing.T) {
	g := newGate441(t)
	var low []taintlineage.ToolCall
	for i := 0; i < taintlineage.MaxSources+1; i++ {
		low = append(low, taintlineage.ToolCall{Tool: "memory_search", Input: fmt.Sprintf(`{"query":"q%02d"}`, i)})
	}
	g.recordCalls("t", low)
	g.recordCalls("t", []taintlineage.ToolCall{{Tool: "web_fetch", Input: `{"url":"https://evil.example/x"}`}})
	tr := g.reviewer(map[string]*persistence.Task{"t": {ID: "t"}}, nil, taintlineage.ModeEnforce)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig == nil {
		t.Fatalf("must park; sig=%v err=%v", sig, err)
	}
	if sig.ShownCount >= sig.SourceCount {
		t.Fatalf("fixture must truncate: shown=%d total=%d", sig.ShownCount, sig.SourceCount)
	}
	for _, s := range sig.Sources {
		if s.Severity == taintlineage.SeverityHigh && s.Tool == "web_fetch" {
			return
		}
	}
	t.Fatalf("the High source must be among the %d shown to the reviewer; shown=%+v", sig.ShownCount, sig.Sources)
}

// Review 20261021-d2c3 finding 4: an ungradable step beside a normal High
// step contributes nothing to the latch key, so an allow granted on the
// visible High source would clear the ungradable step too on re-run. The
// latch must never clear a lineage holding a step nobody could have seen.
func TestTaintGate441_UngradableStepIsNeverLatched(t *testing.T) {
	g := newGate441(t)
	g.recordCalls("t", []taintlineage.ToolCall{{Tool: "web_fetch", Input: `{"url":"https://x.example/a"}`}})
	g.recordStamp("t", taintStamp{UntrustedContentUsed: true, UntrustedSources: []byte(`{"sources":"broken"`)})
	latch := g.parkHash("t")
	if latch == "" {
		t.Fatal("the mixed lineage must park with the visible source's hash")
	}
	tr := g.reviewer(map[string]*persistence.Task{"t": {ID: "t"}}, []string{latch}, taintlineage.ModeEnforce)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig == nil {
		t.Fatalf("a latch for the visible sources must not clear an ungradable step; sig=%v err=%v", sig, err)
	}
}

// A legacy capped blob that HAS a full-set hash is graded Unknown (parks) but
// stays latchable: its key covers the dropped sources and the checkpoint says
// the list is truncated (F-cap), so one allow clears an identical re-run.
func TestTaintGate441_LegacyCappedBlobWithFullHashIsLatchable(t *testing.T) {
	g := newGate441(t)
	var src []taintlineage.Source
	for i := 0; i < taintlineage.MaxSources; i++ {
		src = append(src, taintlineage.Source{Tool: "memory_search", Ref: fmt.Sprintf("q%02d", i), Severity: taintlineage.SeverityLow})
	}
	blob, _ := json.Marshal(map[string]any{"full_hash": "legacyhash", "dropped": 1, "sources": src})
	g.recordStamp("t", taintStamp{UntrustedContentUsed: true, UntrustedSources: blob})
	latch := g.parkHash("t")
	if latch == "" {
		t.Fatal("legacy capped blob must park")
	}
	tr := g.reviewer(map[string]*persistence.Task{"t": {ID: "t"}}, []string{latch}, taintlineage.ModeEnforce)
	if sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t"); err != nil || sig != nil {
		t.Fatalf("an allow on the same legacy set must clear it; sig=%v err=%v", sig, err)
	}
}
