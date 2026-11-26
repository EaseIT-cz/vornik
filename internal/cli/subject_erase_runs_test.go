package cli

import (
	"encoding/json"
	"testing"
)

// GitHub #900, GDPR design §4.6.1: a re-run of an incomplete erasure keeps every
// earlier run's report. earlierReportRuns is the chaining step; the Postgres-lane
// test TestSubjectErase_RerunKeepsEveryRunsReportOnPostgres drives the command.

func decodeRun(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("run is not a JSON object: %v (%s)", err, raw)
	}
	return m
}

func TestEarlierReportRuns_LegacyReportIsRunOne(t *testing.T) {
	// A report stored before #900: no run number, no earlier_runs.
	legacy := `{"subject_narrative":{"summary":"s"},"plan":{"identification_methods":["authenticated"]},"result":{"deleted":[{"row_id":"chat-1"}]}}`
	runs, err := earlierReportRuns(legacy)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%d err=%v, want the legacy report as one run", len(runs), err)
	}
	m := decodeRun(t, runs[0])
	if string(m["run"]) != "1" {
		t.Fatalf("legacy run number = %s, want 1", m["run"])
	}
	var plan struct {
		Methods []string `json:"identification_methods"`
	}
	if err := json.Unmarshal(m["plan"], &plan); err != nil || len(plan.Methods) != 1 || plan.Methods[0] != "authenticated" {
		t.Fatalf("plan not kept verbatim: %s", m["plan"])
	}
	if string(m["result"]) != `{"deleted":[{"row_id":"chat-1"}]}` {
		t.Fatalf("result not kept verbatim: %s", m["result"])
	}
}

func TestEarlierReportRuns_ChainKeepsOrderAndStripsNesting(t *testing.T) {
	stored := `{"run":2,"result":{"deleted":[]},"earlier_runs":[{"run":1,"result":{"deleted":[{"row_id":"chat-1"}]}}]}`
	runs, err := earlierReportRuns(stored)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs=%d err=%v, want two", len(runs), err)
	}
	first, second := decodeRun(t, runs[0]), decodeRun(t, runs[1])
	if string(first["run"]) != "1" || string(second["run"]) != "2" {
		t.Fatalf("order = %s, %s; want 1 then 2", first["run"], second["run"])
	}
	if _, nested := second["earlier_runs"]; nested {
		t.Fatal("the stored run still carries earlier_runs; runs would nest")
	}
}

func TestEarlierReportRuns_RefusesWhatItCannotKeep(t *testing.T) {
	for name, stored := range map[string]string{
		"not json":         `plan: deleted chat-1`,
		"json array":       `[1,2]`,
		"json null":        `null`,
		"earlier not list": `{"run":2,"earlier_runs":{"run":1}}`,
		"earlier number":   `{"run":2,"earlier_runs":[5]}`,
		"earlier null":     `{"run":2,"earlier_runs":[null]}`,
	} {
		if runs, err := earlierReportRuns(stored); err == nil {
			t.Errorf("%s: got %d runs and no error; a report this run cannot keep must refuse the write", name, len(runs))
		}
	}
}

// GitHub #901, GDPR design §4.16.8: one write path serves Art 16, 17 and 18.
// spliceRuns splices "run" first and "earlier_runs" last into the report's
// own encoding, so the report keeps its key order (the narrative first, §7).
func TestSpliceRuns_SplicesRunAroundTheReportsOwnKeyOrder(t *testing.T) {
	report := struct {
		Narrative string `json:"subject_narrative"`
		Plan      string `json:"plan"`
	}{"n", "p"}
	earlier := []json.RawMessage{json.RawMessage(`{"plan":"p0","run":1}`)}
	got, err := spliceRuns(report, earlier)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"run\": 2,\n  \"subject_narrative\": \"n\",\n  \"plan\": \"p\",\n  \"earlier_runs\": [\n    {\n      \"plan\": \"p0\",\n      \"run\": 1\n    }\n  ]\n}"
	if string(got) != want {
		t.Fatalf("document =\n%s\nwant\n%s", got, want)
	}
	first, err := spliceRuns(report, nil)
	if err != nil || string(first) != "{\n  \"run\": 1,\n  \"subject_narrative\": \"n\",\n  \"plan\": \"p\"\n}" {
		t.Fatalf("first run = %s, %v; want run 1 and no earlier_runs", first, err)
	}
}

func TestSpliceRuns_RefusesAReportItCannotSplice(t *testing.T) {
	for name, report := range map[string]any{
		"nil":         nil,
		"list":        []int{1},
		"own run":     map[string]int{"run": 7},
		"own earlier": map[string]int{"earlier_runs": 7},
	} {
		if got, err := spliceRuns(report, nil); err == nil {
			t.Errorf("%s: got %s and no error", name, got)
		}
	}
	if got, err := spliceRuns(struct{}{}, nil); err != nil || string(got) != "{\n  \"run\": 1\n}" {
		t.Errorf("empty report = %s, %v; want run 1 alone", got, err)
	}
}

// The erasure document is byte-identical to the one #900 wrote with its own
// struct, for the same inputs (GDPR design §4.16.8).
func TestSpliceRuns_ErasureDocumentMatchesThe900Shape(t *testing.T) {
	narrative := map[string]string{"summary": "s"}
	plan := map[string][]string{"identification_methods": {"authenticated"}}
	result := map[string][]string{"deleted": {"chat-1"}}
	earlier := []json.RawMessage{json.RawMessage(`{"plan":{},"result":{"deleted":["chat-0"]},"run":1}`)}
	was900, err := json.MarshalIndent(struct {
		Run         int                 `json:"run"`
		Narrative   map[string]string   `json:"subject_narrative"`
		Plan        map[string][]string `json:"plan"`
		Result      map[string][]string `json:"result"`
		EarlierRuns []json.RawMessage   `json:"earlier_runs,omitempty"`
	}{2, narrative, plan, result, earlier}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got, err := spliceRuns(struct {
		Narrative map[string]string   `json:"subject_narrative"`
		Plan      map[string][]string `json:"plan"`
		Result    map[string][]string `json:"result"`
	}{narrative, plan, result}, earlier)
	if err != nil || string(got) != string(was900) {
		t.Fatalf("spliced =\n%s\n(err %v)\nwant the #900 document\n%s", got, err, was900)
	}
}
