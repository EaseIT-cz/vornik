package agentbench

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every task starts from a pristine workspace (benchmark LLD §12.23,
// 2026-09-26). Incident: the slow-hardware arms' dev-swarm tasks asked for NEW
// files under scratch/<pkg>/ that earlier arms had written on 2026-08-14, so
// "task success" measured re-validating finished code, not writing it.

func mustTasks(t *testing.T, js string) []TaskSpec {
	t.Helper()
	var ts []TaskSpec
	if err := json.Unmarshal([]byte(js), &ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestValidateTaskTargets(t *testing.T) {
	ok := mustTasks(t, `[{"id":"a","prompt":"write scratch/x/x.go","targets":["scratch/x/"]},{"id":"b","prompt":"review","targets":[]}]`)
	if err := ValidateTaskTargets(ok, true); err != nil {
		t.Fatalf("declared targets and an explicit empty list are valid: %v", err)
	}
	for name, js := range map[string]string{
		"absolute":      `[{"id":"a","prompt":"/etc/x","targets":["/etc/x"]}]`,
		"dot-dot":       `[{"id":"a","prompt":"../x","targets":["../x"]}]`,
		"not in prompt": `[{"id":"a","prompt":"write scratch/x/x.go","targets":["scratch/y/"]}]`,
	} {
		if err := ValidateTaskTargets(mustTasks(t, js), false); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	missing := mustTasks(t, `[{"id":"a","prompt":"p","targets":[]},{"id":"b","prompt":"p"}]`)
	if err := ValidateTaskTargets(missing, true); err == nil || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("with a workspace, ONE task without the key refuses the set, naming it: %v", err)
	}
	if err := ValidateTaskTargets(missing, false); err != nil {
		t.Fatalf("without a workspace the key is optional: %v", err)
	}
}

type fakeWorkspace struct {
	prepared    []string
	produced    map[string]bool
	err         error
	producedErr error
}

func (f *fakeWorkspace) Prepare(_ context.Context, spec TaskSpec, _ int) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.prepared = append(f.prepared, spec.ID)
	return "c0ffee", nil
}

func (f *fakeWorkspace) Produced(_ context.Context, spec TaskSpec) (map[string]bool, error) {
	if f.producedErr != nil {
		return nil, f.producedErr
	}
	out := map[string]bool{}
	for _, tg := range spec.Targets {
		out[tg] = f.produced[tg]
	}
	return out, nil
}

func TestRunner_PreparesEachRepeatAndRecordsWhatWasProduced(t *testing.T) {
	ws := &fakeWorkspace{produced: map[string]bool{"scratch/x/": false}}
	r := &Runner{Tasks: &fakeTasks{}, Traces: &fakeTraces{}, Workspace: ws}
	cfg := validConfig()
	cfg.Tasks = []TaskSpec{{ID: "t1", Prompt: "write scratch/x/", Targets: []string{"scratch/x/"}}}
	cfg.Repeats = 2
	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.prepared) != 2 {
		t.Fatalf("each repeat must start pristine: prepared %d times, want 2", len(ws.prepared))
	}
	if j.Manifest.WorkspaceReset != WorkspaceResetTargets {
		t.Fatalf("manifest provenance = %q", j.Manifest.WorkspaceReset)
	}
	run := j.TaskRuns[0]
	if run.ClearCommit != "c0ffee" || run.TargetsProduced["scratch/x/"] {
		t.Fatalf("task run must carry the clear commit and what it produced: %+v", run)
	}
	// Succeeded per the daemon, yet produced nothing: visible, not a pass.
	if !run.Succeeded || run.TargetsMissing() != 1 {
		t.Fatalf("a success that produced none of its targets must be visible: %+v", run)
	}
}

func TestRunner_APrepareFailureStopsTheRun(t *testing.T) {
	r := &Runner{Tasks: &fakeTasks{}, Traces: &fakeTraces{}, Workspace: &fakeWorkspace{err: errors.New("dirty")}}
	if _, err := r.Run(context.Background(), validConfig()); err == nil {
		t.Fatal("a task that cannot start pristine must not run")
	}
}

func TestRunner_NoWorkspaceIsRecordedAsSuch(t *testing.T) {
	r := &Runner{Tasks: &fakeTasks{}, Traces: &fakeTraces{}}
	j, err := r.Run(context.Background(), validConfig())
	if err != nil {
		t.Fatal(err)
	}
	if j.Manifest.WorkspaceReset != WorkspaceResetNoWorkspace {
		t.Fatalf("a run without a workspace is not fresh-workspace evidence: %q", j.Manifest.WorkspaceReset)
	}
}

// Every shipped task set carries targets for every task, and a dev-swarm
// task's targets are the scratch paths its prompt names.
func TestShippedTaskSetsDeclareTargets(t *testing.T) {
	files := requireTaskSets(t, "tasksets/*.json") // skips in a CE checkout
	scratch := regexp.MustCompile(`scratch/[\w-]+`)
	n := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var ts []TaskSpec
		if json.Unmarshal(raw, &ts) != nil || len(ts) == 0 || ts[0].Prompt == "" {
			continue // not a task set (gold manifests, fixtures)
		}
		if err := ValidateTaskTargets(ts, true); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		for _, task := range ts {
			for _, dir := range scratch.FindAllString(task.Prompt, -1) {
				found := false
				for _, tg := range task.Targets {
					found = found || tg == dir || strings.HasPrefix(tg, dir+"/")
				}
				if !found {
					t.Errorf("%s: task %s writes %s but does not list it in targets", f, task.ID, dir)
				}
			}
			n++
		}
	}
	if n < 30 {
		t.Fatalf("checked %d tasks; the glob found too few task sets", n)
	}
}

// Gold records the tool path a task needed; batches recorded under different
// workspace resets describe different tasks and must not merge (§12.23).
func TestMergeGold_RefusesMixedWorkspaceResets(t *testing.T) {
	mk := func(reset string) GoldManifest {
		return GoldManifest{TaskSetSHA256: strings.Repeat("a", 64), Runs: 1, WorkspaceReset: reset,
			Entries: []Gold{{TaskID: "t1", Paths: [][]string{{"file_read"}}}}}
	}
	if _, err := MergeGold(mk(WorkspaceResetTargets), mk(WorkspaceResetNoWorkspace)); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("mixed resets must be refused: %v", err)
	}
	m, err := MergeGold(mk(WorkspaceResetTargets), mk(WorkspaceResetTargets))
	if err != nil || m.WorkspaceReset != WorkspaceResetTargets {
		t.Fatalf("matching resets merge and keep the provenance: %+v %v", m.WorkspaceReset, err)
	}
}

// Not knowing what a task produced stops the run instead of reading as a pass
// (review-20260926-b8ae N1).
func TestRunner_AProducedFailureStopsTheRun(t *testing.T) {
	r := &Runner{Tasks: &fakeTasks{}, Traces: &fakeTraces{}, Workspace: &fakeWorkspace{producedErr: errors.New("git broke")}}
	cfg := validConfig()
	cfg.Tasks = []TaskSpec{{ID: "t1", Prompt: "write scratch/x", Targets: []string{"scratch/x"}}}
	if _, err := r.Run(context.Background(), cfg); err == nil {
		t.Fatal("an unreadable result must fail the run, not pass silently")
	}
}
