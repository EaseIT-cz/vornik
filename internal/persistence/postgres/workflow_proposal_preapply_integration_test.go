//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Config-drift slice E (migration 197): the pre-apply snapshot round-trips on
// Postgres byte-exact — it is diffed line by line against the template, so a
// driver that normalised whitespace would silently move every alignment — and
// a plain MarkApplied leaves it NULL, read back as empty (a proposal applied
// before 197, or by a writer that could not read).
func TestWorkflowProposal_PreApplySnapshotRoundTrips(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := NewWorkflowProposalRepository(db.DB)

	pre := "steps:\n  - id: a\n    retry:\n      max: 3\n  - id: b\n\n\t trailing \n"
	// Unique ids: the integration database persists between runs, and fixed
	// ids made this test pass once and fail as "duplicate key" after.
	ids := []string{uniqueSuffix("wpr-pre-1"), uniqueSuffix("wpr-pre-2")}
	for i, withSnapshot := range []bool{true, false} {
		id := ids[i]
		p := &persistence.WorkflowProposal{
			ID: id, WorkflowID: "wf-pre-" + id, Kind: persistence.WorkflowProposalKindRemoveStep,
			ProposalYAML: "steps:\n  - id: b\n", EvidenceRunIDs: []string{"run-1"}, CreatedAt: time.Now().UTC(),
		}
		if err := repo.Insert(ctx, p); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if err := repo.Decide(ctx, id, persistence.WorkflowProposalStatusApproved, "op", ""); err != nil {
			t.Fatalf("Decide: %v", err)
		}
		var err error
		if withSnapshot {
			err = repo.MarkAppliedWithPreApply(ctx, id, "abc1234", []byte(pre))
		} else {
			err = repo.MarkApplied(ctx, id, "abc1234")
		}
		if err != nil {
			t.Fatalf("mark applied: %v", err)
		}
		got, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		want := ""
		if withSnapshot {
			want = pre
		}
		if got.PreApplyYAML != want || got.Status != persistence.WorkflowProposalStatusApplied {
			t.Fatalf("%s: status %s, pre_apply %q, want %q", id, got.Status, got.PreApplyYAML, want)
		}
		list, err := repo.List(ctx, persistence.WorkflowProposalFilter{WorkflowID: p.WorkflowID})
		if err != nil || len(list) != 1 || list[0].PreApplyYAML != want {
			t.Fatalf("%s: List did not carry the snapshot: %v %+v", id, err, list)
		}
	}
	// The stamp is only an approved → applied transition, like MarkApplied.
	if err := repo.MarkAppliedWithPreApply(ctx, ids[0], "def", []byte("x")); err == nil {
		t.Fatal("an applied row was re-stamped")
	}
}
