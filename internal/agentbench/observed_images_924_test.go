package agentbench

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/stepoutcome"
)

// GitHub #924 (bench arm C, 2026-11-25 12:12): an analyst step failed before
// its container started ("failed to write task input: ... disk quota
// exceeded"), so its ledger row has no image and no exit code. The harness
// read that as missing provenance, marked the whole batch PARTIAL, and the
// warm-up merge refused the arm after the last batch.
//
// Found at: production (bench arm C stopped for a human at 14:44).
// Regression test at: unit (the store over the ledger rows of that run).
// Lower because: reproducing needs a full /tmp quota during a live arm; the
// defect is how one row shape is read.

func TestObservedAgentImages_StepThatNeverStartedIsNotARole_924(t *testing.T) {
	db := newLedgerDB(t)
	seedLedger(t, db)
	// A step whose container never ran: no image, classified
	// container_start_failed by the executor from the error's type.
	if _, err := db.Exec(`INSERT INTO execution_step_outcomes
		(execution_id,step_id,role,model,outcome,error_class,recorded_at)
		VALUES ('e1','analyze','analyst','','failed','container_start_failed','2026-11-25T11:12:47Z')`); err != nil {
		t.Fatal(err)
	}
	store := &SQLTraceStore{DB: db, Dialect: SQLite}
	got, err := store.ObservedAgentImages(context.Background(), []string{"e1"})
	if err != nil {
		t.Fatalf("a step that never started a container broke provenance: %v", err)
	}
	if _, ok := got["analyst"]; ok {
		t.Fatalf("a role that never ran is reported as observed: %+v", got)
	}
	if got["lead"] != "sha256:aaa" || got["worker"] != "sha256:bbb" {
		t.Fatalf("the roles that ran are lost: %+v", got)
	}
}

// Any other failed row without an image is still missing provenance: the
// 12:12 rows as written then (unclassified), and a step that ran (it has an
// exit code). The harness re-runs such a batch (§12.39).
func TestObservedAgentImages_StepThatRanWithoutAnImageIsRefused_924(t *testing.T) {
	db := newLedgerDB(t)
	for _, row := range []string{
		`('e1','s1','analyst','','failed','unclassified','2026-11-25T11:12:47Z',NULL)`,
		`('e2','s1','worker','m','failed','degenerate_loop','2026-11-25T11:12:47Z',1)`,
	} {
		if _, err := db.Exec(`INSERT INTO execution_step_outcomes
			(execution_id,step_id,role,model,outcome,error_class,recorded_at,container_exit_code) VALUES ` + row); err != nil {
			t.Fatal(err)
		}
	}
	store := &SQLTraceStore{DB: db, Dialect: SQLite}
	for _, e := range []string{"e1", "e2"} {
		if _, err := store.ObservedAgentImages(context.Background(), []string{e}); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("%s: a row without an image that is not a never-started step must be refused: %v", e, err)
		}
	}
}

func TestClassContainerNeverStarted_MatchesTheExecutor_924(t *testing.T) {
	if classContainerNeverStarted != stepoutcome.ClassContainerStartFailed {
		t.Fatalf("the bench reads %q, the executor writes %q", classContainerNeverStarted, stepoutcome.ClassContainerStartFailed)
	}
}
