package forge

// GitHub #801 B and G at the handler: a mask_secrets step hands the next
// step a masked diff and counts what it masked; without a detector it
// fails; a diff at the provider's read cap is reported as cut. Found at:
// n/a (feature). Tests at: integration (the real handler over a fake
// provider).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/registry"
)

func TestFetchDiffMask801_MaskedCountedAndNamed(t *testing.T) {
	ctx := context.Background()
	prov := &fakeProvider{}
	prov.diff = []byte(diff801)
	task := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4})
	step := &registry.WorkflowStep{MaskSecrets: true}
	h := NewFetchDiffHandler(fakeResolver{p: prov}).WithSecretsDetector(detector801(t))
	res, err := h.Execute(ctx, executor.SystemStepInput{Task: task, Step: step})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(res.Result)
	if strings.Contains(raw, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatal("a value reached the step's result")
	}
	var out struct {
		Message string
		Masked  struct{ Count int }
	}
	_ = json.Unmarshal(res.Result, &out)
	if out.Masked.Count < 2 || !strings.HasPrefix(out.Message, "Vornik masked ") {
		t.Fatalf("masked %d, message %q", out.Masked.Count, out.Message[:60])
	}
	// No detector: fail closed.
	if _, err := NewFetchDiffHandler(fakeResolver{p: prov}).Execute(ctx, executor.SystemStepInput{Task: task, Step: step}); err == nil {
		t.Fatal("masking with no detector succeeded")
	}
	// A diff at the read cap is reported as cut.
	prov.diff = []byte("diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -0,0 +1 @@\n+" + strings.Repeat("y", diffReadCap) + "\n")
	res, err = h.Execute(ctx, executor.SystemStepInput{Task: task, Step: step})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Result), `"truncated":true`) || !strings.Contains(string(res.Result), "a.go") {
		t.Fatalf("a cut diff was not reported: %.200s", res.Result)
	}
}
