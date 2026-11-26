package controlplane

// GitHub #881, registry design §12.2 C2: an apply that changes a role model
// checks it first, before the config lock, whichever proposal kind wrote it.
// A refusal leaves the proposal APPROVED and the file unchanged; an unchanged
// model is not checked.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/configlock"
	"vornik.io/vornik/internal/persistence"
)

const swarmWith = "---\nswarmId: s\ncoordinatorRole: lead\nroles:\n  - name: lead\n    count: 1\n    runtimePolicy: ephemeral\n    model: \"%s\"\n    runtime:\n      image: img\n---\n"

func TestApplyModelCheck_881(t *testing.T) {
	e, repo, dir := multiOpEnv(t)
	if err := os.MkdirAll(filepath.Join(dir, "swarms"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "swarms", "s.md")
	if err := os.WriteFile(path, []byte(strings.Replace(swarmWith, "%s", "old-model", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var checked [][]string
	lockFree := true
	e.ModelCheck = func(_ context.Context, changed []string) ([]string, error) {
		if unlock, ok := configlock.TryLock(); ok {
			unlock()
		} else {
			lockFree = false
		}
		checked = append(checked, changed)
		for _, m := range changed {
			if m == "dead-model" {
				return nil, errors.New("dead-model did not answer: gateway error 410")
			}
		}
		return nil, nil
	}

	id := seedScaffold(t, repo, []applyFileOp{{Op: applyOpReplace, Path: "swarms/s.md", Content: strings.Replace(swarmWith, "%s", "dead-model", 1)}})
	err := e.Apply(context.Background(), id, "vadim", true)
	if err == nil || !strings.Contains(err.Error(), "dead-model") {
		t.Fatalf("apply err = %v, want the model refusal", err)
	}
	if !lockFree {
		t.Error("the model check ran under the config lock")
	}
	if got := readFile(t, path); !strings.Contains(got, "old-model") {
		t.Errorf("a refused apply changed the swarm: %s", got)
	}
	if p, _ := repo.GetByID(context.Background(), id); p.Status != persistence.ProposalStatusApproved {
		t.Errorf("status = %s, want still APPROVED so it can be retried", p.Status)
	}

	// An op that keeps the model checks nothing; a good new model applies.
	checked = nil
	id = seedScaffold(t, repo, []applyFileOp{{Op: applyOpReplace, Path: "swarms/s.md", Content: strings.Replace(swarmWith, "%s", "old-model", 1) + "\n"}})
	if err := e.Apply(context.Background(), id, "vadim", true); err != nil {
		t.Fatalf("unchanged model: %v", err)
	}
	if len(checked) != 0 {
		t.Errorf("checked %v, want no call: no model changed", checked)
	}
}

// RenderRoleModel refuses a model that does not answer, after KnownModel.
func TestRenderRoleModel_881_Unreachable(t *testing.T) {
	a := &Actionizer{
		KnownModel:     func(string) bool { return true },
		ReachableModel: func(string) error { return errors.New("gateway error 410: retired") },
		ReadFile:       func(string) ([]byte, error) { return []byte(strings.Replace(swarmWith, "%s", "old-model", 1)), nil },
	}
	if _, err := a.RenderRoleModel("s", "lead", "dead-model"); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err = %v", err)
	}
}
