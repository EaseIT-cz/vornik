package context

import (
	stdctx "context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/registry"
)

func msg801(s string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"message": s})
	return b
}

func TestGather801_ReemitsInOrderAndFailsLoud(t *testing.T) {
	in := executor.SystemStepInput{Step: &registry.WorkflowStep{Gather: []string{"hunt_a", "hunt_b"}},
		StepResults: map[string]json.RawMessage{"hunt_a": msg801("list A"), "hunt_b": msg801("list B")}}
	res, err := GatherHandler{}.Execute(stdctx.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var env struct{ Message string }
	_ = json.Unmarshal(res.Result, &env)
	if a, b := strings.Index(env.Message, "list A"), strings.Index(env.Message, "list B"); a < 0 || b < a {
		t.Fatalf("message = %q", env.Message)
	}
	in.Step.Gather = []string{"hunt_a", "nope"}
	if _, err := (GatherHandler{}).Execute(stdctx.Background(), in); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("a missing step = %v", err)
	}
	in.Step.Gather = []string{"big", "hunt_b"}
	in.StepResults["big"] = msg801(strings.Repeat("x", gatherBudget))
	if _, err := (GatherHandler{}).Execute(stdctx.Background(), in); err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("an oversized gather = %v", err)
	}
}
