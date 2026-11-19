package autonomy

// GitHub #369 Q15b (outcome-quality design, Amendment 2026-11-18 (#369),
// item 2; operator decision B): only a cancel with a recorded cause
// (last_error, e.g. an approval timeout) counts toward autonomy's circuit; an
// operator's bare cancel is dropped before the window is cut. Found at:
// review (the #142 audit); observed at: integration. Test at: unit; Lower
// because: the rule is a pure function of the recent tasks.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func caused369(t *persistence.Task) *persistence.Task {
	cause := "approval timed out: nobody approved or rejected this task within 24h0m0s, so it was cancelled"
	t.LastError = &cause
	return t
}

func TestCircuit369_BareCancelsDoNotTripCausedOnesDo(t *testing.T) {
	var bare, caused []*persistence.Task
	for i := 0; i < 8; i++ {
		bare = append(bare, mkTerminal(fmt.Sprintf("b%d", i), persistence.TaskStatusCancelled, time.Duration(i)*time.Minute))
		caused = append(caused, caused369(mkTerminal(fmt.Sprintf("c%d", i), persistence.TaskStatusCancelled, time.Duration(i)*time.Minute)))
	}
	if _, open := autonomyCircuitOpen(bare); open {
		t.Fatal("eight bare operator cancels tripped the circuit")
	}
	reason, open := autonomyCircuitOpen(caused)
	if !open || !strings.Contains(reason, "8 cancelled with a cause") {
		t.Fatalf("eight cancels with a cause: open %v, reason %q", open, reason)
	}
}

func TestCircuit369_BareCancelsTakeNoSlotInTheWindow(t *testing.T) {
	var tasks []*persistence.Task
	for i := 0; i < 20; i++ { // newest: twenty bare cancels
		tasks = append(tasks, mkTerminal(fmt.Sprintf("b%d", i), persistence.TaskStatusCancelled, time.Duration(i)*time.Second))
	}
	for i := 0; i < 12; i++ { // older: twelve failures
		tasks = append(tasks, mkTerminal(fmt.Sprintf("f%d", i), persistence.TaskStatusFailed, time.Hour+time.Duration(i)*time.Minute))
	}
	reason, open := autonomyCircuitOpen(tasks)
	if !open {
		t.Fatal("twelve failures behind twenty bare cancels did not trip: the bare cancels took the window")
	}
	if !strings.Contains(reason, "12 failed") || !strings.Contains(reason, "20 bare operator cancels left out") {
		t.Fatalf("reason %q must name the failures and the bare cancels left out", reason)
	}
}
