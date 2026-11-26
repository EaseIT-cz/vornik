package cli

// GitHub #809 (Amendment 2026-11-18 (#809), the revision): the harness
// autopilot against a fake bench daemon. Found at: n/a (feature). Test at:
// integration (the command over HTTP against an httptest daemon).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/agentbench"
)

type fakeBench809 struct {
	mu           sync.Mutex
	frozen       bool
	openTrig     bool
	noCand       bool
	genStatus    int    // GitHub #908: when set, generate answers this status
	trialVerdict string // GitHub #921: when set, the candidate's trial status
	calls        []string
	candidate    string
	sha          string
}

func (f *fakeBench809) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.URL.Path == "/api/v1/admin/bench/learned-state":
		cands := []map[string]string{}
		if f.candidate != "" {
			cands = append(cands, map[string]string{"id": f.candidate, "trigger_id": "trg-1", "status": "draft"})
		}
		trigs := []map[string]string{}
		if f.openTrig {
			trigs = append(trigs, map[string]string{"id": "trg-0", "workflow_id": "dev-pipeline", "status": "open"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"trigger_enabled": true, "auto": true, "frozen": f.frozen, "candidates": cands, "triggers": trigs, "sha256": f.sha})
	case r.URL.Path == "/api/v1/admin/bench/freeze":
		f.frozen = true
		_ = json.NewEncoder(w).Encode(map[string]any{"frozen_at": "2026-11-18T12:00:00Z"})
	case r.URL.Path == "/api/v1/admin/bench/triggers":
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"trigger_id": "trg-1"})
	case strings.HasSuffix(r.URL.Path, "/generate-candidate"):
		if f.genStatus != 0 {
			w.WriteHeader(f.genStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "HEALING_PRODUCER_FAILED", "message": "configassist: assistant model: context deadline exceeded"}})
			return
		}
		if f.noCand {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "ASSISTANT_DECLINED", "message": "no change warranted"}})
			return
		}
		f.candidate = "cand-1"
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "trg-1", "status": "generated_candidate"})
	case strings.HasSuffix(r.URL.Path, "/run-trial"):
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"verdict": "pending"})
	case strings.HasSuffix(r.URL.Path, "/dismiss"):
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "dismissed"})
	case strings.HasSuffix(r.URL.Path, "/promote"):
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "promoted", "candidate_genome_hash": "g-1"})
	case strings.HasSuffix(r.URL.Path, "/reject"):
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "rejected"})
	case r.URL.Path == "/api/v1/admin/workflow-healing/candidates/cand-1":
		v := "trial_passed"
		if f.trialVerdict != "" {
			v = f.trialVerdict
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"candidate": map[string]string{"status": v}})
	default:
		http.NotFound(w, r)
	}
}

func setupAutopilot809(t *testing.T) (string, *fakeBench809) {
	t.Helper()
	dir := t.TempDir()
	tasks := `[{"id":"pg-go-a","workflow":"dev-pipeline"},{"id":"pg-go-b","workflow":"dev-pipeline"}]`
	if err := os.WriteFile(filepath.Join(dir, "warmup.json"), []byte(tasks), 0o600); err != nil {
		t.Fatal(err)
	}
	runs := []agentbench.TaskRun{}
	for i, ok := range []bool{true, false, false, true, false} {
		runs = append(runs, agentbench.TaskRun{TaskID: "pg-go-a", Repeat: i, Succeeded: ok, ExecutionIDs: []string{"exec-" + string(rune('a'+i))}})
	}
	j := agentbench.Journal{Manifest: agentbench.RunManifest{RunID: "w", Phase: agentbench.PhaseWarmup}, TaskRuns: runs}
	raw, _ := json.Marshal(j)
	runDir := filepath.Join(dir, "run")
	_ = os.MkdirAll(runDir, 0o700)
	if err := os.WriteFile(filepath.Join(runDir, "arm-taskbatch-0.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "bench:\n  instance: true\n  learning:\n    auto: true\n    trigger:\n      enabled: true\n      min_samples: 5\n      threshold: 0.3\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeBench809{sha: "s-1"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	benchAutopilotRunDir, benchAutopilotTasks, benchAgentDaemonConfig = runDir, filepath.Join(dir, "warmup.json"), filepath.Join(dir, "config.yaml")
	benchAutopilotPoll = 0
	t.Cleanup(func() { benchAutopilotRunDir, benchAutopilotTasks, benchAgentDaemonConfig = "", "", "" })
	return runDir, f
}

func TestAutopilot809_RunsTheOperatorsStepsAndJournalsThem(t *testing.T) {
	runDir, f := setupAutopilot809(t)
	var out bytes.Buffer
	benchAgentAutopilotCmd.SetOut(&out)
	if err := runBenchAgentAutopilot(benchAgentAutopilotCmd, nil); err != nil {
		t.Fatalf("autopilot: %v\n%s", err, out.String())
	}
	var st agentbench.AutopilotState
	raw, err := os.ReadFile(filepath.Join(runDir, "autopilot.json"))
	if err != nil || json.Unmarshal(raw, &st) != nil {
		t.Fatalf("autopilot.json: %v", err)
	}
	var steps []string
	for _, e := range st.Events {
		steps = append(steps, e.Step+":"+e.Verdict)
	}
	if got := strings.Join(steps, ","); got != "intent:,trigger:,generate:,trial:trial_passed,promote:promoted" {
		t.Fatalf("steps = %s; calls %v", got, f.calls)
	}
	if st.Events[4].GenomeHash != "g-1" || st.Workflows["dev-pipeline"].RunsAtLastTrigger != 5 {
		t.Fatalf("promotion not journaled with its hash: %+v", st)
	}
	// Run again: no new runs since the trigger, so nothing is due.
	f.calls = nil
	if err := runBenchAgentAutopilot(benchAgentAutopilotCmd, nil); err != nil || len(f.calls) != 0 {
		t.Fatalf("a second run with no new runs: %v, calls %v", err, f.calls)
	}
}

func TestAutopilot809_StopsWhenFrozen(t *testing.T) {
	runDir, f := setupAutopilot809(t)
	f.frozen = true
	benchAgentAutopilotCmd.SetOut(&bytes.Buffer{})
	if err := runBenchAgentAutopilot(benchAgentAutopilotCmd, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "/bench/triggers") {
			t.Fatalf("a trigger was written while frozen: %v", f.calls)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(runDir, "autopilot.json"))
	if !strings.Contains(string(raw), "learning is frozen") {
		t.Fatalf("the stop was not journaled: %s", raw)
	}
}

func TestLearnedState809_VerifyRefusesAMovedTreatment(t *testing.T) {
	_, f := setupAutopilot809(t)
	benchAgentFreezeCmd.SetOut(&bytes.Buffer{})
	if err := benchAgentFreezeCmd.RunE(benchAgentFreezeCmd, nil); err != nil {
		t.Fatal(err)
	}
	benchLearnedVerify = true
	t.Cleanup(func() { benchLearnedVerify = false })
	benchAgentLearnedStateCmd.SetOut(&bytes.Buffer{})
	if err := benchAgentLearnedStateCmd.RunE(benchAgentLearnedStateCmd, nil); err != nil {
		t.Fatalf("an unchanged state was refused: %v", err)
	}
	f.sha = "s-2"
	if err := benchAgentLearnedStateCmd.RunE(benchAgentLearnedStateCmd, nil); err == nil || !strings.Contains(err.Error(), "treatment changed") {
		t.Fatalf("a moved treatment: %v", err)
	}
}

func autopilotSteps809(t *testing.T, runDir string) string {
	t.Helper()
	var st agentbench.AutopilotState
	raw, _ := os.ReadFile(filepath.Join(runDir, "autopilot.json"))
	_ = json.Unmarshal(raw, &st)
	var steps []string
	for _, e := range st.Events {
		steps = append(steps, e.Step+":"+e.Verdict)
	}
	return strings.Join(steps, ",")
}

func TestAutopilot809_AnOpenTriggerIsNotDuplicated(t *testing.T) {
	runDir, f := setupAutopilot809(t)
	f.openTrig = true
	benchAgentAutopilotCmd.SetOut(&bytes.Buffer{})
	if err := runBenchAgentAutopilot(benchAgentAutopilotCmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := autopilotSteps809(t, runDir); got != "skip:" {
		t.Fatalf("steps = %s, want a skip and no trigger; calls %v", got, f.calls)
	}
}

func TestAutopilot809_NoCandidateDismissesTheTrigger(t *testing.T) {
	runDir, f := setupAutopilot809(t)
	f.noCand = true
	benchAgentAutopilotCmd.SetOut(&bytes.Buffer{})
	if err := runBenchAgentAutopilot(benchAgentAutopilotCmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := autopilotSteps809(t, runDir); got != "intent:,trigger:,generate:no_candidate,dismiss:dismissed" {
		t.Fatalf("steps = %s; calls %v", got, f.calls)
	}
}

// GitHub #921: a failed trial left its proposal pending and blocked every
// later candidate for the workflow. The autopilot rejects the failed
// candidate, which frees the proposal, and journals it.
//
// Found at: production (bench arm C, 2026-11-25: "workflow already has a
// pending proposal" on the next generate).
// Regression test at: unit (the autopilot against a fake bench daemon; the
// daemon half in workflowhealing TestReject_FreesThePendingProposal_921).
// Lower because: the arm needs hours of warm-up to reach a failed trial.
func TestAutopilot921_AFailedTrialRejectsTheCandidate(t *testing.T) {
	runDir, f := setupAutopilot809(t)
	f.trialVerdict = "trial_failed"
	var out bytes.Buffer
	benchAgentAutopilotCmd.SetOut(&out)
	if err := runBenchAgentAutopilot(benchAgentAutopilotCmd, nil); err != nil {
		t.Fatalf("autopilot: %v\n%s", err, out.String())
	}
	var st agentbench.AutopilotState
	raw, err := os.ReadFile(filepath.Join(runDir, "autopilot.json"))
	if err != nil || json.Unmarshal(raw, &st) != nil {
		t.Fatalf("autopilot.json: %v", err)
	}
	var steps []string
	for _, e := range st.Events {
		steps = append(steps, e.Step+":"+e.Verdict)
	}
	if got := strings.Join(steps, ","); got != "intent:,trigger:,generate:,trial:trial_failed,reject:rejected" {
		t.Fatalf("steps = %s; calls %v", got, f.calls)
	}
	rejected := false
	for _, c := range f.calls {
		if c == "POST /api/v1/admin/workflow-healing/candidates/cand-1/reject" {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("no reject call: %v", f.calls)
	}
}
