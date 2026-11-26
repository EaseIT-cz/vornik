package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #912 (g426b bench, 2026-11-24): the counterfactual engine stamped
// a replay's provenance on its execution row at task creation, before the
// scheduler had created the row, so 8 of 8 replays lost the link. The
// executor now stamps it right after it creates the row.
//
// Found at: e2e-lane (bench daemon log, "no execution row yet").
// Regression test at: unit (the executor's stamp step over the payload).
// Lower because: the engine half is in blackbox/engine
// TestEngine_Apply_NeverStampsAtCreation_912; the Postgres UPDATE is the
// existing StampCounterfactualOnExecution.

type stampCall struct{ execution, orig, variable, label string }

func stampExecutor(err error) (*Executor, *[]stampCall) {
	calls := &[]stampCall{}
	e := &Executor{logger: zerolog.Nop()}
	WithCounterfactualStamper(func(_ context.Context, execution, orig, variable, label string) error {
		*calls = append(*calls, stampCall{execution, orig, variable, label})
		return err
	})(e)
	return e, calls
}

func TestStampCounterfactual_StampsAReplay_912(t *testing.T) {
	e, calls := stampExecutor(nil)
	task := &persistence.Task{ID: "task_cf", Payload: json.RawMessage(`{"context":{"counterfactual":{"provenance":{"original_task_id":"task_orig","variable":"model","label":"swap"}}}}`)}
	e.stampCounterfactual(context.Background(), task, &persistence.Execution{ID: "exec_1", TaskID: "task_cf"})
	if len(*calls) != 1 || (*calls)[0] != (stampCall{"exec_1", "task_orig", "model", "swap"}) {
		t.Fatalf("stamp calls = %+v", *calls)
	}
}

func TestStampCounterfactual_LeavesANativeTaskAlone_912(t *testing.T) {
	e, calls := stampExecutor(nil)
	task := &persistence.Task{ID: "task_1", Payload: json.RawMessage(`{"context":{"prompt":"x"}}`)}
	e.stampCounterfactual(context.Background(), task, &persistence.Execution{ID: "exec_1", TaskID: "task_1"})
	if len(*calls) != 0 {
		t.Fatalf("a native task was stamped: %+v", *calls)
	}
}

// A stamp error is a warning: the execution runs anyway. Without a stamper
// (SQLite, which has no such columns) nothing happens.
func TestStampCounterfactual_ErrorAndNoStamperAreHarmless_912(t *testing.T) {
	e, calls := stampExecutor(errors.New("db down"))
	task := &persistence.Task{ID: "task_cf", Payload: json.RawMessage(`{"context":{"counterfactual":{"provenance":{"original_task_id":"o","variable":"model"}}}}`)}
	e.stampCounterfactual(context.Background(), task, &persistence.Execution{ID: "exec_1"})
	if len(*calls) != 1 {
		t.Fatalf("the stamper was not tried: %+v", *calls)
	}
	bare := &Executor{logger: zerolog.Nop()}
	bare.stampCounterfactual(context.Background(), task, &persistence.Execution{ID: "exec_1"})
	if bare.counterfactualStamper != nil {
		t.Fatal("no stamper must stay no stamper")
	}
}
