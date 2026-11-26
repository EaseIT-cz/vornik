package counterfactual

import (
	"encoding/json"
	"testing"
)

// GitHub #912: a replay task carries its provenance in
// context.counterfactual.provenance, so the executor can stamp it on the
// execution row it creates.
func TestExtractPayload_ReadsProvenance_912(t *testing.T) {
	p := ExtractPayload(json.RawMessage(`{"context":{"counterfactual":{"provenance":{"original_task_id":"task_orig","variable":"model","label":"swap"}}}}`))
	if !p.IsReplay {
		t.Fatal("a payload with a counterfactual block is a replay")
	}
	if want := (Provenance{OriginalTaskID: "task_orig", Variable: "model", Label: "swap"}); p.Provenance != want {
		t.Fatalf("provenance = %+v, want %+v", p.Provenance, want)
	}
	if !p.Provenance.Known() {
		t.Fatal("a provenance with an original task and a variable is known")
	}
	if (Provenance{Variable: "model"}).Known() || (Provenance{OriginalTaskID: "t"}).Known() {
		t.Fatal("a provenance missing the original task or the variable is not known")
	}
	if ExtractPayload(json.RawMessage(`{"context":{"prompt":"x"}}`)).Provenance.Known() {
		t.Fatal("a native task has no provenance")
	}
}
