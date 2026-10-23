package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
)

// TraceCacheSeeder writes and counts blackbox_trace_cache rows. Supported is
// false on a driver that has no such table (SQLite); the suite then asserts the
// erasure says "not applicable" and is complete.
type TraceCacheSeeder interface {
	Supported() bool
	SeedAssembledTrace(ctx context.Context, taskID, projectID, payloadJSON string) error
	CountAssembledTraces(ctx context.Context, taskID string) (int, error)
}

// TraceCacheErasureStore is the store surface the suite needs: the data-subject
// store, which is also the row deleter and the trace-cache evictor.
type TraceCacheErasureStore interface {
	DataSubjectStore
	EvictAssembledTraces(ctx context.Context) (evicted int, applicable bool, err error)
}

type noArtifacts struct{}

func (noArtifacts) EraseArtifact(context.Context, string) (datasubject.ArtifactErasureCounts, error) {
	return datasubject.ArtifactErasureCounts{}, nil
}

// RunDataSubjectTraceCacheEvictionSuite runs an erasure end to end on a real
// database and checks the assembled-trace cache afterwards.
//
// GitHub #415 item 3 (design 2026-07-29-art17-redact-and-reembed §15.12): an
// erasure left every cached black-box trace (a JSONB copy of task messages and
// audit rows) in place, served for 24 h and kept until the retention sweep (for
// ever with retention.enabled off). Found at: review; a user observes it at
// integration, as the row still present after `subject erase` reports complete.
// So this runs the executor exactly as the CLI wires it (Rows is the store, no
// Traces field) against the real tables, on both drivers.
func RunDataSubjectTraceCacheEvictionSuite(t *testing.T, s TraceCacheErasureStore, seedChat SeedChatTurn, seed TraceCacheSeeder) {
	t.Helper()
	ctx := context.Background()
	subj := mustSubject(ctx, t, s, "trace-cache")
	proj := uniqueID("proj")
	turn := uniqueID("chat")
	if err := seedChat(ctx, turn, proj, "the subject's own message"); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	link := chatLink(turn, proj, datasubject.SourceAuthenticated, datasubject.ConfidenceCertain)
	if err := s.AddLink(ctx, subj, link); err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	// Two cached traces: one of the task the turn started, one of an unrelated
	// task. Both go, because no query can say which traces hold the subject's
	// data (§15.12 item 3).
	related, unrelated := uniqueID("task"), uniqueID("task")
	if seed.Supported() {
		for _, id := range []string{related, unrelated} {
			if err := seed.SeedAssembledTrace(ctx, id, proj,
				`{"header":{"task_id":"`+id+`"},"events":[{"text":"the subject's own message"}]}`); err != nil {
				t.Fatalf("seed trace: %v", err)
			}
		}
	}

	items, err := s.CollectItems(ctx, subj, []datasubject.Link{link})
	if err != nil {
		t.Fatalf("CollectItems: %v", err)
	}
	now := time.Now()
	plan, err := datasubject.PlanErasure(datasubject.Request{
		ID: uniqueID("req"), SubjectID: subj, Kind: datasubject.RequestErasure,
		State: datasubject.StateVerified, OpenedAt: now,
		VerifiedBy: "operator", VerifiedHow: "channel session", VerifiedAt: now,
		ErasureGround: datasubject.GroundConsentWithdrawn,
	}, items, []string{"authenticated_identity"})
	if err != nil {
		t.Fatalf("PlanErasure: %v", err)
	}
	ex := &datasubject.Executor{Rows: s, Artifacts: noArtifacts{}}
	res, err := ex.Execute(ctx, plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Complete() || res.RowsDeleted != 1 {
		t.Fatalf("erasure: complete=%v rows=%d failed=%+v", res.Complete(), res.RowsDeleted, res.Failed)
	}
	if !seed.Supported() {
		if res.AssembledTraceCache != datasubject.TraceCacheNotApplicable {
			t.Fatalf("a driver without the cache must say %q, got %q",
				datasubject.TraceCacheNotApplicable, res.AssembledTraceCache)
		}
		return
	}
	if res.AssembledTraceCache != datasubject.TraceCacheEvicted || res.AssembledTracesEvicted < 2 {
		t.Fatalf("cache = %q evicted = %d, want %q and at least the 2 seeded",
			res.AssembledTraceCache, res.AssembledTracesEvicted, datasubject.TraceCacheEvicted)
	}
	for _, id := range []string{related, unrelated} {
		n, err := seed.CountAssembledTraces(ctx, id)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("cached trace %s survived the erasure: a copy of erased data still served", id)
		}
	}
}
