package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Broker design §17 (2026-10-03): task task_20261003003901_067399b47e7fc6f2
// ran three steps and lost the whole answer at result() because its summary
// was 603 characters against maxLength 600. The answering step's answer is
// now judged at the step, with the same validator, so the existing one-shot
// corrective shape retry can fix it.

type egressResolver struct{ wf *registry.Workflow }

func (r egressResolver) GetProject(string) *registry.Project { return nil }
func (r egressResolver) GetSwarm(string) *registry.Swarm     { return nil }
func (r egressResolver) GetWorkflow(id string) *registry.Workflow {
	if r.wf != nil && r.wf.ID == id {
		return r.wf
	}
	return nil
}

func gapsBrokerWF() *registry.Workflow {
	return &registry.Workflow{ID: "ns--p--gaps", Entrypoint: "draft",
		Steps:     map[string]registry.WorkflowStep{"draft": {OnSuccess: "edit"}, "edit": {OnSuccess: "done"}},
		Terminals: map[string]registry.WorkflowTerminal{"done": {}},
		Broker: &registry.WorkflowBroker{InputSchema: map[string]any{"type": "object"},
			Egress: registry.BrokerEgress{Output: "result.json", Schema: map[string]any{
				"type": "object", "required": []any{"summary"},
				"properties": map[string]any{"summary": map[string]any{"type": "string", "maxLength": float64(600)}},
			}}},
	}
}

func egressOutcome(t *testing.T, wf *registry.Workflow, step, answer string) (*Executor, *StepOutcome) {
	t.Helper()
	ws := t.TempDir()
	start := time.Now().Add(-time.Second)
	if answer != "" {
		dir := filepath.Join(ws, "artifacts", "out")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(answer), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := &Executor{workflows: egressResolver{wf}}
	return e, &StepOutcome{StepID: step, Step: &registry.WorkflowStep{}, StepStart: start, WorkspaceDir: ws,
		Execution: &persistence.Execution{WorkflowID: wf.ID}}
}

func TestBrokerEgressParticipant(t *testing.T) {
	ctx := context.Background()
	long := `{"summary":"` + strings.Repeat("x", 603) + `"}`

	e, in := egressOutcome(t, gapsBrokerWF(), "edit", long)
	v := e.brokerEgressParticipant(ctx, in)
	if !v.Refused || !strings.HasPrefix(v.Reason, "schema violation:") || !strings.Contains(v.Reason, "/summary: maxLength (600 allowed, 603 written)") {
		t.Fatalf("603-character answer: %+v", v)
	}
	if classifyShapeFailure(errors.New(v.Reason)) == shapeFailureNone {
		t.Fatal("the refusal is not a shape failure, so no corrective turn")
	}

	e, in = egressOutcome(t, gapsBrokerWF(), "edit", "")
	if v := e.brokerEgressParticipant(ctx, in); !v.Refused || !strings.Contains(v.Reason, "was not written") ||
		classifyShapeFailure(errors.New(v.Reason)) == shapeFailureNone {
		t.Fatalf("absent answer: %+v", v)
	}

	e, in = egressOutcome(t, gapsBrokerWF(), "edit", `{"summary":"ok"}`)
	if v := e.brokerEgressParticipant(ctx, in); v.Refused {
		t.Fatalf("valid answer refused: %+v", v)
	}

	// An intermediate step is not judged, even with an invalid file present.
	e, in = egressOutcome(t, gapsBrokerWF(), "draft", long)
	if v := e.brokerEgressParticipant(ctx, in); v.Refused {
		t.Fatalf("intermediate step judged: %+v", v)
	}

	// A non-broker workflow is untouched.
	plain := gapsBrokerWF()
	plain.Broker = nil
	e, in = egressOutcome(t, plain, "edit", long)
	if v := e.brokerEgressParticipant(ctx, in); v.Refused {
		t.Fatalf("non-broker workflow judged: %+v", v)
	}

	// Oversize is correctable too (§17 settled points).
	big := gapsBrokerWF()
	big.Broker.Egress.MaxBytes = 16
	e, in = egressOutcome(t, big, "edit", `{"summary":"0123456789"}`)
	if v := e.brokerEgressParticipant(ctx, in); !v.Refused || !strings.Contains(v.Reason, "byte limit") ||
		classifyShapeFailure(errors.New(v.Reason)) == shapeFailureNone {
		t.Fatalf("oversize answer: %+v", v)
	}
}
