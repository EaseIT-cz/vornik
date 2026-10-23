package taintlineage

// GitHub #441 (security audit 2026-10-17): severity classes are OR-ed, never
// decided by the maximum. Unit cases under the executor-gate tests in
// internal/executor/taint_severity_441_test.go. Design §15.

import (
	"encoding/json"
	"testing"
)

func TestClassify441_HighPlusUnknownKeepsHighFlag(t *testing.T) {
	st := Classify([]ToolCall{{Tool: "web_fetch", Input: `{"url":"https://a.example"}`}, {Tool: "mystery_tool"}})
	if st.MaxSeverity != SeverityUnknown || !st.RequiresReview {
		t.Fatalf("High+Unknown: max=%v requires_review=%v, want Unknown/true", st.MaxSeverity, st.RequiresReview)
	}
	blob, _ := json.Marshal(NewSourcesBlob(st))
	var b SourcesBlob
	if err := json.Unmarshal(blob, &b); err != nil || b.MaxSeverity != SeverityUnknown {
		t.Fatalf("blob must record max_severity=Unknown, got %+v err=%v", b, err)
	}
}

func TestRollup441_OrsClassesFromSources(t *testing.T) {
	// A pre-#441 row: stored flag false, sources hold High and Unknown.
	blob := []byte(`[{"tool":"web_fetch","ref":"u","severity":2},{"tool":"m","ref":"m","severity":3}]`)
	roll := Rollup([]StepTaint{StepTaintFromBlob(blob, false)}, nil, true)
	if !roll.RequiresReview || !roll.HasUnknown || roll.Ungradable {
		t.Fatalf("rollup = %+v, want RequiresReview and HasUnknown, gradable", roll)
	}
}

func TestStepTaintFromBlob441_Ungradable(t *testing.T) {
	for name, blob := range map[string]string{
		"nil":        ``,
		"garbage":    `{"sources":`,
		"empty":      `[]`,
		"sev-none":   `[{"tool":"x","severity":0}]`,
		"sev-7":      `[{"tool":"x","severity":7}]`,
		"max-9":      `{"max_severity":9,"sources":[{"tool":"x","severity":1}]}`,
		"capped-raw": `{"dropped":2,"sources":[{"tool":"x","severity":1}]}`,
	} {
		st := StepTaintFromBlob([]byte(blob), false)
		if !st.Ungradable {
			t.Errorf("%s: must be ungradable", name)
		}
		roll := Rollup([]StepTaint{st}, nil, true)
		if !roll.HasUnknown || !roll.Ungradable {
			t.Errorf("%s: rollup must carry Unknown+Ungradable, got %+v", name, roll)
		}
	}
	st := StepTaintFromBlob([]byte(`{"full_hash":"h","dropped":2,"sources":[{"tool":"x","severity":1}]}`), false)
	if st.Ungradable || st.MaxSeverity != SeverityUnknown {
		t.Fatalf("legacy capped blob with full hash: want gradable Unknown, got %+v", st)
	}
}

func TestHashSources441_SeverityInKey(t *testing.T) {
	a := HashSources([]Source{{Tool: "t", Ref: "r", Severity: SeverityUnknown}})
	b := HashSources([]Source{{Tool: "t", Ref: "r", Severity: SeverityHigh}})
	if a == "" || a == b {
		t.Fatalf("same source at different severities must hash differently: %q %q", a, b)
	}
	// Demotion too (review e2f2): High → Low changes the key.
	if c := HashSources([]Source{{Tool: "t", Ref: "r", Severity: SeverityLow}}); c == b {
		t.Fatal("a demoted source must not match the latch for its High grading")
	}
}

// A healthy classified step is never ungradable (guards against false parks).
func TestGradeStep441_HealthyStepIsGradable(t *testing.T) {
	st := Classify([]ToolCall{{Tool: "web_fetch", Input: `{"url":"https://a.example"}`}, {Tool: "memory_search", Input: `{"query":"q"}`}})
	if _, _, ug := gradeStep(st); ug {
		t.Fatal("a classified step must be gradable")
	}
	blob, _ := json.Marshal(NewSourcesBlob(st))
	if _, _, ug := gradeStep(StepTaintFromBlob(blob, st.RequiresReview)); ug {
		t.Fatal("a round-tripped classified step must be gradable")
	}
}

func TestDecide441_UngradableNeverLatched(t *testing.T) {
	roll := TaskTaint{Tainted: true, RequiresReview: true, HasUnknown: true, Ungradable: true,
		WalkComplete: true, SourceSetHash: "h"}
	if d := Decide(ModeEnforce, roll, []string{"h"}); !d.Park {
		t.Fatal("a matching latch must not clear an ungradable lineage")
	}
	roll.Ungradable = false
	if d := Decide(ModeEnforce, roll, []string{"h"}); d.Park {
		t.Fatal("control: a gradable lineage with a matching latch is cleared (D7)")
	}
}

func TestClassify441_CapKeepsMostSevere(t *testing.T) {
	var calls []ToolCall
	for i := 0; i < MaxSources; i++ {
		calls = append(calls, ToolCall{Tool: "memory_search", Input: `{"query":"q` + string(rune('a'+i)) + `"}`})
	}
	calls = append(calls, ToolCall{Tool: "mystery_tool"})
	st := Classify(calls)
	if st.DroppedSources != 1 || st.Sources[0].Severity != SeverityUnknown {
		t.Fatalf("cap must keep the Unknown source first; dropped=%d first=%+v", st.DroppedSources, st.Sources[0])
	}
}
