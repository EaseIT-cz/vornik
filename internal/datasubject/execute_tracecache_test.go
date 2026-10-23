package datasubject

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The assembled-trace cache (GitHub #415 item 3, design
// 2026-07-29-art17-redact-and-reembed §15.12).
//
// An erasure left every cached black-box trace (a JSONB copy of task messages
// and audit rows) in place: served for 24 h and kept until the retention sweep,
// for ever with retention off. Bounded staleness is not erasure. The executor
// now evicts the cache once, after every action. The end-to-end guarantee on a
// real database is the repotest suite RunDataSubjectTraceCacheEvictionSuite
// (both drivers); these pin the executor's ordering and failure policy.

// fakeTraceEvictor records when it ran relative to the row deletes.
type fakeTraceEvictor struct {
	rows       *fakeRowDeleter
	ranAfter   int // len(rows.deleted) when it ran
	calls      int
	evicted    int
	applicable bool
	err        error
}

func (f *fakeTraceEvictor) EvictAssembledTraces(context.Context) (int, bool, error) {
	f.calls++
	if f.rows != nil {
		f.ranAfter = len(f.rows.deleted)
	}
	return f.evicted, f.applicable, f.err
}

// rowsWithEvictor is a row deleter that is also the evictor, as the subject
// store is in production.
type rowsWithEvictor struct {
	fakeRowDeleter
	ev fakeTraceEvictor
}

func (r *rowsWithEvictor) EvictAssembledTraces(ctx context.Context) (int, bool, error) {
	return r.ev.EvictAssembledTraces(ctx)
}

func oneChatRow(t *testing.T, ids ...string) *ErasurePlan {
	t.Helper()
	items := make([]Item, 0, len(ids))
	for _, id := range ids {
		items = append(items, Item{Table: TableChatAuditLog, RowID: id, Exclusivity: ExclusiveRow})
	}
	return planFor(t, GroundConsentWithdrawn, items)
}

func TestExecuteErasure_EvictsAssembledTracesAfterTheActions(t *testing.T) {
	rows := &fakeRowDeleter{}
	ev := &fakeTraceEvictor{rows: rows, evicted: 3, applicable: true}
	ex := &Executor{Rows: rows, Artifacts: &fakeArtifactEraser{}, Traces: ev}
	res, err := ex.Execute(context.Background(), oneChatRow(t, "c1", "c2"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ev.calls != 1 || ev.ranAfter != 2 {
		t.Fatalf("evictor must run once after both deletes, calls=%d ranAfter=%d", ev.calls, ev.ranAfter)
	}
	if res.AssembledTracesEvicted != 3 || res.AssembledTraceCache != TraceCacheEvicted {
		t.Fatalf("result = %d %q, want 3 %q", res.AssembledTracesEvicted, res.AssembledTraceCache, TraceCacheEvicted)
	}
	if !res.Complete() {
		t.Fatal("a clean eviction must leave the erasure complete")
	}
}

// A failed eviction leaves a copy of the erased data in place: the erasure is
// incomplete and the caller gets ErrPartialErasure, as for a failed row.
func TestExecuteErasure_FailedTraceEvictionIsAnIncompleteErasure(t *testing.T) {
	ex := &Executor{Rows: &fakeRowDeleter{}, Artifacts: &fakeArtifactEraser{},
		Traces: &fakeTraceEvictor{applicable: true, err: errors.New("relation does not exist")}}
	res, err := ex.Execute(context.Background(), oneChatRow(t, "c1"))
	if !errors.Is(err, ErrPartialErasure) {
		t.Fatalf("want ErrPartialErasure, got %v", err)
	}
	if res.Complete() || res.AssembledTraceCache != TraceCacheFailed {
		t.Fatalf("complete=%v cache=%q, want incomplete and %q", res.Complete(), res.AssembledTraceCache, TraceCacheFailed)
	}
	if len(res.Failed) != 1 || res.Failed[0].Table != TableBlackboxTraceCache ||
		!strings.Contains(res.Failed[0].Err, "relation does not exist") {
		t.Fatalf("failure not recorded against the cache: %+v", res.Failed)
	}
}

// Rows that changed before a row failed may be in a cached trace, so a partial
// erasure still evicts.
func TestExecuteErasure_PartialErasureStillEvicts(t *testing.T) {
	rows := &fakeRowDeleter{failOn: map[string]error{string(TableChatAuditLog) + "/c2": errors.New("boom")}}
	ev := &fakeTraceEvictor{applicable: true}
	ex := &Executor{Rows: rows, Artifacts: &fakeArtifactEraser{}, Traces: ev}
	if _, err := ex.Execute(context.Background(), oneChatRow(t, "c1", "c2")); !errors.Is(err, ErrPartialErasure) {
		t.Fatalf("want ErrPartialErasure, got %v", err)
	}
	if ev.calls != 1 {
		t.Fatalf("a partial erasure must still evict, calls=%d", ev.calls)
	}
}

// The production wiring: Rows is the subject store, which is also the evictor,
// and no Traces field is set.
func TestExecuteErasure_RowsThatEvictAreUsedWhenTracesIsNil(t *testing.T) {
	rows := &rowsWithEvictor{ev: fakeTraceEvictor{applicable: true, evicted: 1}}
	ex := &Executor{Rows: rows, Artifacts: &fakeArtifactEraser{}}
	res, err := ex.Execute(context.Background(), oneChatRow(t, "c1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rows.ev.calls != 1 || res.AssembledTraceCache != TraceCacheEvicted || res.AssembledTracesEvicted != 1 {
		t.Fatalf("the row store's evictor was not used: calls=%d cache=%q n=%d",
			rows.ev.calls, res.AssembledTraceCache, res.AssembledTracesEvicted)
	}
}

// A driver without the table is not a failure, and says so.
func TestExecuteErasure_TraceCacheNotApplicableOnADriverWithoutIt(t *testing.T) {
	ex := &Executor{Rows: &fakeRowDeleter{}, Artifacts: &fakeArtifactEraser{}, Traces: &fakeTraceEvictor{applicable: false}}
	res, err := ex.Execute(context.Background(), oneChatRow(t, "c1"))
	if err != nil || !res.Complete() || res.AssembledTraceCache != TraceCacheNotApplicable {
		t.Fatalf("err=%v cache=%q", err, res.AssembledTraceCache)
	}
}

// An executor with no evictor at all reports it rather than staying silent.
func TestExecuteErasure_NoEvictorIsReportedNotWired(t *testing.T) {
	ex := &Executor{Rows: &fakeRowDeleter{}, Artifacts: &fakeArtifactEraser{}}
	res, err := ex.Execute(context.Background(), oneChatRow(t, "c1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.AssembledTraceCache != TraceCacheNotWired {
		t.Fatalf("cache = %q, want %q", res.AssembledTraceCache, TraceCacheNotWired)
	}
}
