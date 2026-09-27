package chat

import (
	"context"
	"testing"
)

// Chat memory-write design §12 (review F9a): Action.Confirm is set only by the
// dispatcher after an authorized two-step. Model-emitted action JSON must not
// be able to set it, so ExecuteAction refuses such an action.
func TestParseActions_ModelCannotSetConfirm(t *testing.T) {
	actions := ParseActions("```json\n{\"action\":\"cancel_task\",\"task_id\":\"t1\",\"confirm\":true}\n```")
	if len(actions) != 1 {
		t.Fatalf("want one parsed action, got %d", len(actions))
	}
	if actions[0].Confirm {
		t.Fatal("model-emitted JSON set Confirm — the destructive gate is bypassable again")
	}
	res, err := ExecuteAction(context.Background(), actions[0], nil, nil, nil)
	if err != nil || !res.NeedsConfirmation {
		t.Fatalf("an action parsed from model text must be refused as unconfirmed: %+v %v", res, err)
	}
}
