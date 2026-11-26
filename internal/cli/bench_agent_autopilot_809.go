package cli

// GitHub #809 (agent-benchmark design, Amendment 2026-11-18 (#809), the
// revision): `vornikctl bench agent autopilot`, the blackbox arms' auto mode.
// It runs an operator's three steps through the bench daemon's admin routes,
// with its admin key, so it can do nothing an operator could not:
//
//	create a bench trigger -> generate a candidate -> replay-trial it -> promote it
//
// Promotion is the shipped route, which refuses any candidate whose trial
// did not pass. Before every step it reads the learned state and stops when
// learning is frozen or the daemon no longer runs the bench trigger and the
// auto mode. Every step is journaled to autopilot.json in the run directory.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/config"
)

var (
	benchAutopilotRunDir    string
	benchAutopilotTasks     string
	benchAutopilotProject   string
	benchAutopilotTrialWait time.Duration
	benchAutopilotPoll      = 10 * time.Second
)

var benchAgentAutopilotCmd = &cobra.Command{
	Use:   "autopilot",
	Short: "Run the blackbox arm's auto mode once: trigger, generate, replay-trial, promote",
	Long: `Reads the warm-up journals in --run-dir, finds each workflow whose failure
rate since its last trigger is at or above bench.learning.trigger.threshold
(with at least min_samples runs), and runs an operator's three steps for it
through the bench daemon's admin routes: create a bench trigger, generate a
candidate, replay-trial it on the failed executions, and promote it when the
trial passed. Needs VORNIK_API_URL (the bench daemon) and VORNIK_API_KEY (its
admin key). Stops when learning is frozen. Journals every step to
<run-dir>/autopilot.json.`,
	Args: cobra.NoArgs,
	RunE: runBenchAgentAutopilot,
}

func init() {
	benchAgentAutopilotCmd.Flags().StringVar(&benchAutopilotRunDir, "run-dir", "", "the arm's run directory (warm-up journals, autopilot.json)")
	benchAgentAutopilotCmd.Flags().StringVar(&benchAutopilotTasks, "tasks", "", "the warm-up task set, for each task's workflow")
	benchAgentAutopilotCmd.Flags().StringVar(&benchAutopilotProject, "project", "agentbench", "the benchmark project")
	benchAgentAutopilotCmd.Flags().StringVar(&benchAgentDaemonConfig, "daemon-config", "", "the bench daemon's config, for bench.learning.trigger")
	benchAgentAutopilotCmd.Flags().DurationVar(&benchAutopilotTrialWait, "trial-wait", 45*time.Minute, "how long to wait for one replay trial's verdict")
	benchAgentCmd.AddCommand(benchAgentAutopilotCmd)
}

func runBenchAgentAutopilot(cmd *cobra.Command, _ []string) error {
	if benchAutopilotRunDir == "" || benchAutopilotTasks == "" || benchAgentDaemonConfig == "" {
		return fmt.Errorf("--run-dir, --tasks and --daemon-config are required")
	}
	tuning, err := benchTriggerTuning(benchAgentDaemonConfig)
	if err != nil {
		return err
	}
	runs, err := warmupRuns(benchAutopilotRunDir, benchAutopilotTasks)
	if err != nil {
		return err
	}
	statePath := filepath.Join(benchAutopilotRunDir, "autopilot.json")
	st, err := readAutopilotState(statePath)
	if err != nil {
		return err
	}
	gen, err := autopilotGenerateClient(benchAgentDaemonConfig)
	if err != nil {
		return err
	}
	ap := &autopilot{c: ClientFromEnv(), gen: gen, state: st, path: statePath, out: cmd.OutOrStdout()}
	due := agentbench.DueTriggers(runs, *st, tuning.MinSamples, tuning.Threshold)
	if len(due) == 0 {
		_, _ = fmt.Fprintf(ap.out, "autopilot: %d warm-up runs, no workflow due a trigger\n", len(runs))
	}
	for _, d := range due {
		if err := ap.repair(d); err != nil {
			return err
		}
	}
	// The arm's summary, every run: an arm whose generate calls all failed
	// must read as such, not as "nothing to fix" (GitHub #908).
	_, _ = fmt.Fprintln(ap.out, agentbench.SummarizeAutopilot(*ap.state).String())
	return nil
}

// autopilotGenerateClient is the generate call's client. Its deadline is the
// daemon's generate budget (config_assistant.request_timeout plus the margin)
// plus 1 minute. GitHub #908: the 30 s default gave up first.
func autopilotGenerateClient(daemonConfig string) (*Client, error) {
	raw, err := os.ReadFile(filepath.Clean(daemonConfig))
	if err != nil {
		return nil, fmt.Errorf("read daemon config: %w", err)
	}
	var doc struct {
		ConfigAssistant config.AssistantConfig `yaml:"config_assistant"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse daemon config: %w", err)
	}
	return ClientFromEnv().WithMinTimeout(doc.ConfigAssistant.HealingGenerateBudget() + generateClientSlack), nil
}

// benchTriggerTuning reads bench.learning.trigger from the daemon config.
func benchTriggerTuning(path string) (config.BenchLearningTrigger, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return config.BenchLearningTrigger{}, fmt.Errorf("read daemon config: %w", err)
	}
	var doc struct {
		Bench config.BenchConfig `yaml:"bench"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return config.BenchLearningTrigger{}, fmt.Errorf("parse daemon config: %w", err)
	}
	return doc.Bench.Learning.Trigger.Resolved(), nil
}

// warmupRuns reads the warm-up journals in run order (file modification
// time), mapping each task to its workflow from the task set.
func warmupRuns(dir, tasksPath string) ([]agentbench.AutopilotRun, error) {
	raw, err := os.ReadFile(filepath.Clean(tasksPath))
	if err != nil {
		return nil, fmt.Errorf("read the warm-up task set: %w", err)
	}
	var tasks []agentbench.TaskSpec
	if err := json.Unmarshal(raw, &tasks); err != nil {
		return nil, fmt.Errorf("parse the warm-up task set: %w", err)
	}
	workflowOf := map[string]string{}
	for _, t := range tasks {
		workflowOf[t.ID] = t.Workflow
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	type dated struct {
		path string
		at   time.Time
	}
	var files []dated
	for _, p := range paths {
		if filepath.Base(p) == "autopilot.json" || filepath.Base(p) == "prereg.json" {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		files = append(files, dated{p, fi.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	var runs []agentbench.AutopilotRun
	unknown := 0
	for _, f := range files {
		fh, err := os.Open(f.path)
		if err != nil {
			return nil, err
		}
		j, err := agentbench.ReadJournal(fh)
		_ = fh.Close()
		if err != nil || j.Manifest.Phase != agentbench.PhaseWarmup {
			continue // not a warm-up journal
		}
		for _, tr := range j.TaskRuns {
			wf := workflowOf[tr.TaskID]
			if wf == "" {
				// Not in the warm-up set given: never grouped under "".
				unknown++
				continue
			}
			runs = append(runs, agentbench.AutopilotRun{Workflow: wf, Succeeded: tr.Succeeded, ExecutionIDs: tr.ExecutionIDs})
		}
	}
	if unknown > 0 {
		_, _ = fmt.Fprintf(os.Stderr, "autopilot: %d warm-up run(s) name a task that is not in %s; they are left out\n", unknown, tasksPath)
	}
	return runs, nil
}

func readAutopilotState(path string) (*agentbench.AutopilotState, error) {
	st := &agentbench.AutopilotState{Workflows: map[string]*agentbench.AutopilotWorkflow{}}
	raw, err := os.ReadFile(filepath.Clean(path))
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Workflows == nil {
		st.Workflows = map[string]*agentbench.AutopilotWorkflow{}
	}
	return st, nil
}

type autopilot struct {
	c     *Client
	gen   *Client // the generate call's client: outlives the daemon's generate budget (GitHub #908)
	state *agentbench.AutopilotState
	path  string
	out   interface{ Write([]byte) (int, error) }
}

// promotedWire is one promoted candidate as the learned-state route reports it.
type promotedWire struct {
	CandidateID string     `json:"candidate_id"`
	WorkflowID  string     `json:"workflow_id"`
	GenomeHash  string     `json:"genome_hash"`
	FileSHA256  string     `json:"file_sha256"`
	PromotedAt  *time.Time `json:"promoted_at,omitempty"`
}

// learnedStateWire is the part of the learned-state route the autopilot reads.
type learnedStateWire struct {
	TriggerEnabled bool `json:"trigger_enabled"`
	Auto           bool `json:"auto"`
	Frozen         bool `json:"frozen"`
	Candidates     []struct {
		ID        string `json:"id"`
		TriggerID string `json:"trigger_id"`
		Status    string `json:"status"`
	} `json:"candidates"`
	Promoted []promotedWire `json:"promoted"`
	Triggers []struct {
		ID         string `json:"id"`
		WorkflowID string `json:"workflow_id"`
		Status     string `json:"status"`
	} `json:"triggers"`
}

// call sends one request and decodes a 2xx answer into out (when non-nil).
func (a *autopilot) call(method, path string, body, out any) error {
	resp, err := a.c.Do(method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ParseAPIError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// check reads the learned state before a step: it refuses to go on when
// learning is frozen or this arm does not run the bench trigger and auto mode.
func (a *autopilot) check() (learnedStateWire, error) {
	var st learnedStateWire
	if err := a.call(http.MethodGet, "/api/v1/admin/bench/learned-state", nil, &st); err != nil {
		return st, fmt.Errorf("read the learned state: %w", err)
	}
	if st.Frozen || !st.TriggerEnabled || !st.Auto {
		return st, errAutopilotStop{frozen: st.Frozen}
	}
	return st, nil
}

type errAutopilotStop struct{ frozen bool }

func (e errAutopilotStop) Error() string {
	if e.frozen {
		return "learning is frozen"
	}
	return "this arm does not run the bench trigger and the auto mode"
}

func (a *autopilot) event(e agentbench.AutopilotEvent) error {
	e.At = time.Now().UTC()
	a.state.Events = append(a.state.Events, e)
	_, _ = fmt.Fprintf(a.out, "autopilot: %s %s %s %s %s\n", e.Step, e.Workflow, e.TriggerID, e.CandidateID, e.Verdict)
	raw, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.path, append(raw, '\n'), 0o600)
}

// repair runs the four steps for one due workflow.
func (a *autopilot) repair(d agentbench.AutopilotDue) error {
	stop := func(err error) error {
		if s, ok := err.(errAutopilotStop); ok {
			return a.event(agentbench.AutopilotEvent{Step: "stop", Workflow: d.Workflow, TaskRuns: d.RunsTotal, Detail: s.Error()})
		}
		return err
	}
	pre, err := a.check()
	if err != nil {
		return stop(err)
	}
	for _, t := range pre.Triggers {
		if t.WorkflowID == d.Workflow && t.Status == "open" {
			return a.event(agentbench.AutopilotEvent{Step: "skip", Workflow: d.Workflow, TriggerID: t.ID, TaskRuns: d.RunsTotal,
				Detail: "an open bench trigger exists for this workflow"})
		}
	}
	trigID, err := a.openTrigger(d)
	if err != nil {
		return err
	}
	trig := struct{ TriggerID string }{trigID}
	if _, err := a.check(); err != nil {
		return stop(err)
	}
	if err := a.generate(trig.TriggerID); err != nil {
		return a.noCandidate(d, trig.TriggerID, generateVerdict(err), err.Error())
	}
	st, err := a.check()
	if err != nil {
		return stop(err)
	}
	cand := ""
	for _, c := range st.Candidates {
		if c.TriggerID == trig.TriggerID {
			cand = c.ID
		}
	}
	if cand == "" {
		return a.noCandidate(d, trig.TriggerID, "no_candidate", "")
	}
	if err := a.event(agentbench.AutopilotEvent{Step: "generate", Workflow: d.Workflow, TriggerID: trig.TriggerID, CandidateID: cand, TaskRuns: d.RunsTotal}); err != nil {
		return err
	}
	if err := a.call(http.MethodPost, "/api/v1/admin/workflow-healing/candidates/"+cand+"/run-trial",
		map[string]any{"mode": "replay", "evidence_execution_ids": d.EvidenceExecutionIDs}, nil); err != nil {
		return a.event(agentbench.AutopilotEvent{Step: "trial", Workflow: d.Workflow, TriggerID: trig.TriggerID, CandidateID: cand, TaskRuns: d.RunsTotal, Verdict: "not_run", Detail: err.Error()})
	}
	verdict, err := a.awaitTrial(cand)
	if err != nil {
		return err
	}
	if err := a.event(agentbench.AutopilotEvent{Step: "trial", Workflow: d.Workflow, TriggerID: trig.TriggerID, CandidateID: cand, TaskRuns: d.RunsTotal, Verdict: verdict}); err != nil {
		return err
	}
	if verdict == "trial_failed" {
		// A failed candidate's proposal holds the workflow's one pending slot
		// until the candidate is rejected; without this every later trigger
		// for the workflow failed "workflow already has a pending proposal"
		// (GitHub #921).
		if err := a.call(http.MethodPost, "/api/v1/admin/workflow-healing/candidates/"+cand+"/reject", map[string]any{}, nil); err != nil {
			return a.event(agentbench.AutopilotEvent{Step: "reject", Workflow: d.Workflow, TriggerID: trig.TriggerID, CandidateID: cand, TaskRuns: d.RunsTotal, Verdict: "refused", Detail: err.Error()})
		}
		return a.event(agentbench.AutopilotEvent{Step: "reject", Workflow: d.Workflow, TriggerID: trig.TriggerID, CandidateID: cand, TaskRuns: d.RunsTotal, Verdict: "rejected"})
	}
	if verdict != "trial_passed" {
		return nil
	}
	if _, err := a.check(); err != nil {
		return stop(err)
	}
	var promoted struct {
		CandidateGenomeHash string `json:"candidate_genome_hash"`
		Status              string `json:"status"`
	}
	if err := a.call(http.MethodPost, "/api/v1/admin/workflow-healing/candidates/"+cand+"/promote", map[string]any{}, &promoted); err != nil {
		return a.event(agentbench.AutopilotEvent{Step: "promote", Workflow: d.Workflow, TriggerID: trig.TriggerID, CandidateID: cand, TaskRuns: d.RunsTotal, Verdict: "refused", Detail: err.Error()})
	}
	return a.event(agentbench.AutopilotEvent{Step: "promote", Workflow: d.Workflow, TriggerID: trig.TriggerID, CandidateID: cand,
		GenomeHash: promoted.CandidateGenomeHash, TaskRuns: d.RunsTotal, Verdict: promoted.Status})
}

// openTrigger records the intent, creates the bench trigger and journals it.
// The intent comes first: a crash after it costs at most a missed trigger,
// never a duplicate one.
func (a *autopilot) openTrigger(d agentbench.AutopilotDue) (string, error) {
	a.state.Workflows[d.Workflow] = &agentbench.AutopilotWorkflow{RunsAtLastTrigger: d.RunsTotal}
	if err := a.event(agentbench.AutopilotEvent{Step: "intent", Workflow: d.Workflow, TaskRuns: d.RunsTotal}); err != nil {
		return "", err
	}
	var trig struct {
		TriggerID string `json:"trigger_id"`
	}
	if err := a.call(http.MethodPost, "/api/v1/admin/bench/triggers", map[string]any{
		"project_id": benchAutopilotProject, "workflow_id": d.Workflow, "failure_rate": d.FailureRate,
		"runs": d.Runs, "evidence_execution_ids": d.EvidenceExecutionIDs,
	}, &trig); err != nil {
		return "", fmt.Errorf("create the bench trigger: %w", err)
	}
	a.state.Workflows[d.Workflow].LastTriggerID = trig.TriggerID
	if err := a.event(agentbench.AutopilotEvent{Step: "trigger", Workflow: d.Workflow, TriggerID: trig.TriggerID, TaskRuns: d.RunsTotal,
		Detail: fmt.Sprintf("failure rate %.2f over %d runs", d.FailureRate, d.Runs)}); err != nil {
		return "", err
	}
	return trig.TriggerID, nil
}

// generate posts generate-candidate on the generate client.
func (a *autopilot) generate(triggerID string) error {
	c := a.gen
	if c == nil {
		c = a.c
	}
	resp, err := c.Do(http.MethodPost, "/api/v1/admin/workflow-healing/triggers/"+triggerID+"/generate-candidate", map[string]any{})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ParseAPIError(resp)
	}
	return nil
}

// generateVerdict classifies a failed generate call (GitHub #908). Only the
// producer's own "made nothing" answers are no_candidate: 422 (the
// assistant's edit is not a single workflow edit) and 409 (the trigger is no
// longer open). Everything else is failed: no answer (a transport error or
// client timeout), a 5xx, and a 4xx such as 401, 404 or 429, which say the
// call never reached the producer. None of those is "nothing to fix".
func generateVerdict(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnprocessableEntity || apiErr.StatusCode == http.StatusConflict) {
		return "no_candidate"
	}
	return "failed"
}

// noCandidate journals a trigger that produced no candidate (verdict
// no_candidate or failed) and dismisses it, so it does not stay open and
// block the workflow's next trigger.
func (a *autopilot) noCandidate(d agentbench.AutopilotDue, triggerID, verdict, detail string) error {
	if err := a.event(agentbench.AutopilotEvent{Step: "generate", Workflow: d.Workflow, TriggerID: triggerID, TaskRuns: d.RunsTotal, Verdict: verdict, Detail: detail}); err != nil {
		return err
	}
	if err := a.call(http.MethodPost, "/api/v1/admin/workflow-healing/triggers/"+triggerID+"/dismiss", map[string]any{}, nil); err != nil {
		return a.event(agentbench.AutopilotEvent{Step: "dismiss", Workflow: d.Workflow, TriggerID: triggerID, TaskRuns: d.RunsTotal, Verdict: "failed", Detail: err.Error()})
	}
	return a.event(agentbench.AutopilotEvent{Step: "dismiss", Workflow: d.Workflow, TriggerID: triggerID, TaskRuns: d.RunsTotal, Verdict: "dismissed"})
}

// awaitTrial polls the candidate until its replay trial has a verdict.
func (a *autopilot) awaitTrial(cand string) (string, error) {
	deadline := time.Now().Add(benchAutopilotTrialWait)
	for {
		var c struct {
			Candidate struct {
				Status string `json:"status"`
			} `json:"candidate"`
			Status string `json:"status"`
		}
		if err := a.call(http.MethodGet, "/api/v1/admin/workflow-healing/candidates/"+cand, nil, &c); err != nil {
			return "", fmt.Errorf("read candidate %s: %w", cand, err)
		}
		status := c.Candidate.Status
		if status == "" {
			status = c.Status
		}
		switch strings.TrimSpace(status) {
		case "trial_passed", "trial_failed", "rejected", "promoted":
			return status, nil
		}
		if time.Now().After(deadline) {
			return "trial_timeout", nil
		}
		time.Sleep(benchAutopilotPoll)
	}
}

// The freeze around the measured pass (GitHub #809, change 3): `freeze` stops
// learning and records the learned state; `learned-state --verify` after the
// measured pass records it again and fails if the treatment changed.

var benchLearnedVerify bool

var benchAgentFreezeCmd = &cobra.Command{
	Use:   "freeze",
	Short: "Freeze learning before the measured pass and record the learned state",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if benchAutopilotRunDir == "" {
			return fmt.Errorf("--run-dir is required")
		}
		c := ClientFromEnv()
		ap := &autopilot{c: c}
		if err := ap.call(http.MethodPost, "/api/v1/admin/bench/freeze", map[string]any{}, nil); err != nil {
			return fmt.Errorf("freeze: %w", err)
		}
		sum, err := saveLearnedState(ap, filepath.Join(benchAutopilotRunDir, "learned-state-freeze.json"))
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "learning frozen; learned state %s\n", sum)
		return nil
	},
}

var benchAgentLearnedStateCmd = &cobra.Command{
	Use:   "learned-state",
	Short: "Record the learned state; with --verify, fail if it moved since the freeze",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if benchLearnedCheckPhase != "" || benchLearnedPromotedFiles {
			if benchLearnedVerify || (benchLearnedCheckPhase != "" && benchLearnedPromotedFiles) {
				return fmt.Errorf("--check-phase, --promoted-files and --verify are separate modes; give one")
			}
			return runLearnedStateReadOnly(cmd)
		}
		if benchAutopilotRunDir == "" {
			return fmt.Errorf("--run-dir is required")
		}
		ap := &autopilot{c: ClientFromEnv()}
		sum, err := saveLearnedState(ap, filepath.Join(benchAutopilotRunDir, "learned-state-after.json"))
		if err != nil {
			return err
		}
		if !benchLearnedVerify {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "learned state %s\n", sum)
			return nil
		}
		var frozen struct {
			SHA256 string `json:"sha256"`
		}
		raw, err := os.ReadFile(filepath.Join(benchAutopilotRunDir, "learned-state-freeze.json"))
		if err != nil {
			return fmt.Errorf("no learned state recorded at the freeze: %w", err)
		}
		if err := json.Unmarshal(raw, &frozen); err != nil {
			return err
		}
		if frozen.SHA256 != sum {
			return fmt.Errorf("treatment changed during the measured pass: learned state %s at the freeze, %s after; the arm is refused", frozen.SHA256, sum)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "learned state unchanged since the freeze (%s)\n", sum)
		return nil
	},
}

func saveLearnedState(ap *autopilot, path string) (string, error) {
	var st json.RawMessage
	if err := ap.call(http.MethodGet, "/api/v1/admin/bench/learned-state", nil, &st); err != nil {
		return "", fmt.Errorf("read the learned state: %w", err)
	}
	var sum struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(st, &sum); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append([]byte(st), '\n'), 0o600); err != nil {
		return "", err
	}
	return sum.SHA256, nil
}

func init() {
	for _, c := range []*cobra.Command{benchAgentFreezeCmd, benchAgentLearnedStateCmd} {
		c.Flags().StringVar(&benchAutopilotRunDir, "run-dir", "", "the arm's run directory")
		benchAgentCmd.AddCommand(c)
	}
	benchAgentLearnedStateCmd.Flags().BoolVar(&benchLearnedVerify, "verify", false, "fail if the learned state moved since the freeze")
	benchAgentLearnedStateCmd.Flags().StringVar(&benchLearnedCheckPhase, "check-phase", "", "warmup or measure: refuse a pass whose phase does not match the freeze; reads --daemon-config, writes nothing")
	benchAgentLearnedStateCmd.Flags().BoolVar(&benchLearnedPromotedFiles, "promoted-files", false, "print each promoted workflow and the SHA-256 of its latest promoted file, for preflight check 3c; writes nothing")
	benchAgentLearnedStateCmd.Flags().StringVar(&benchAgentDaemonConfig, "daemon-config", "", "the bench daemon's config, for the arm's learning switches (with --check-phase)")
}

var (
	benchLearnedCheckPhase    string
	benchLearnedPromotedFiles bool
)

// runLearnedStateReadOnly serves --check-phase and --promoted-files (GitHub
// #890, design §12.37): read the learned state, write nothing.
func runLearnedStateReadOnly(cmd *cobra.Command) error {
	ap := &autopilot{c: ClientFromEnv()}
	var st learnedStateWire
	readErr := ap.call(http.MethodGet, "/api/v1/admin/bench/learned-state", nil, &st)
	if benchLearnedPromotedFiles {
		if readErr != nil {
			return fmt.Errorf("read the learned state: %w", readErr)
		}
		_, _ = fmt.Fprint(cmd.OutOrStdout(), latestPromotedFiles(st))
		return nil
	}
	if benchAgentDaemonConfig == "" {
		return fmt.Errorf("--check-phase needs --daemon-config, for the arm's learning switches")
	}
	raw, err := os.ReadFile(benchAgentDaemonConfig)
	if err != nil {
		return fmt.Errorf("read --daemon-config: %w", err)
	}
	_, learning, err := agentbench.ArmConfigIdentity(raw)
	if err != nil {
		return err
	}
	on := learning.Instincts || learning.BlackboxTrigger || learning.Auto
	if err := checkLearningPhase(benchLearnedCheckPhase, on, st, readErr); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "learning phase %s: ok (learning on: %v, frozen: %v)\n", benchLearnedCheckPhase, on, st.Frozen)
	return nil
}

// checkLearningPhase decides whether a pass may start. A warm-up must not
// start frozen (the #890 incident: a freeze outlived its arm); a learning
// arm's measured pass must start frozen; an arm with no learning switch on
// has nothing to check, and its daemon answers 404. A learning arm whose
// state cannot be read is refused: its gate did not open, so learning would
// run unfrozen.
func checkLearningPhase(phase string, learningOn bool, st learnedStateWire, readErr error) error {
	if phase != "warmup" && phase != "measure" {
		return fmt.Errorf("--check-phase must be warmup or measure, not %q", phase)
	}
	if !learningOn {
		return nil
	}
	if readErr != nil {
		var apiErr *APIError
		if errors.As(readErr, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return fmt.Errorf("the arm's learning switches are on but the bench daemon has no learned state (404): its freeze gate did not open, see the daemon log")
		}
		return fmt.Errorf("cannot read the learned state of a learning arm: %w", readErr)
	}
	if phase == "warmup" && st.Frozen {
		return fmt.Errorf("learning is frozen at the start of a warm-up: this arm would learn nothing. A freeze from an earlier arm? See the bench daemon log for the freeze marker")
	}
	if phase == "measure" && !st.Frozen {
		return fmt.Errorf("learning is not frozen at the start of the measured pass: freeze first (vornikctl bench agent freeze)")
	}
	return nil
}

// latestPromotedFiles prints "<workflow> <file_sha256>" for each workflow's
// latest promotion, sorted by workflow; "-" when the route had no hash, which
// check 3c then refuses.
func latestPromotedFiles(st learnedStateWire) string {
	latest := map[string]promotedWire{}
	for _, p := range st.Promoted {
		cur, seen := latest[p.WorkflowID]
		if !seen || (p.PromotedAt != nil && (cur.PromotedAt == nil || p.PromotedAt.After(*cur.PromotedAt))) {
			latest[p.WorkflowID] = p
		}
	}
	ids := make([]string, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	for _, id := range ids {
		sum := latest[id].FileSHA256
		if sum == "" {
			sum = "-"
		}
		fmt.Fprintf(&b, "%s %s\n", id, sum)
	}
	return b.String()
}
