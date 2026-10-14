package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

// RunMemoryPolicyEvaluationSuite pins the memory firewall's evaluation audit on
// both drivers: a batch round-trips every column (blank optional fields read
// back blank), ListRecent is project- and window-scoped, newest first, with an
// optional exact decision filter and the limit honoured; ListByDigest and
// ListByChunk return exactly their key's rows, newest first; an empty batch is
// a no-op.
//
// audit 2026-10-12 (GitHub #355): on SQLite the repository was a stub that
// discarded every row, so the firewall's audit trail read empty there whatever
// had been evaluated.
func RunMemoryPolicyEvaluationSuite(t *testing.T, repo persistence.MemoryPolicyEvaluationRepository) {
	t.Helper()
	ctx := context.Background()
	projA, projB := uniqueID("proj"), uniqueID("proj")
	digest, chunk := uniqueID("digest"), uniqueID("chunk")
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	if err := repo.BatchInsert(ctx, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	full := memoryfirewall.EvaluationRow{
		ID: uniqueID("ev"), ProjectID: projA, TenantID: "t1", ChunkID: chunk,
		RequestRole: "researcher", RequestPurpose: "support", RequestOperator: "op",
		TraceID: "trace-1", Decision: memoryfirewall.DecisionBlockExpired,
		PolicyDigest: digest, ReasonDetail: "expired", EvaluatedAt: base.Add(3 * time.Minute),
	}
	rows := []memoryfirewall.EvaluationRow{
		full,
		{ID: uniqueID("ev"), ProjectID: projA, ChunkID: chunk, Decision: memoryfirewall.DecisionAllow,
			PolicyDigest: digest, EvaluatedAt: base.Add(2 * time.Minute)},
		{ID: uniqueID("ev"), ProjectID: projA, ChunkID: uniqueID("chunk"), Decision: memoryfirewall.DecisionAllow,
			EvaluatedAt: base.Add(time.Minute)},
		{ID: uniqueID("ev"), ProjectID: projA, ChunkID: uniqueID("chunk"), Decision: memoryfirewall.DecisionAllow,
			EvaluatedAt: base.Add(-48 * time.Hour)}, // outside the window below
		{ID: uniqueID("ev"), ProjectID: projB, ChunkID: chunk, Decision: memoryfirewall.DecisionAllow,
			PolicyDigest: digest, EvaluatedAt: base.Add(4 * time.Minute)},
	}
	if err := repo.BatchInsert(ctx, rows); err != nil {
		t.Fatalf("BatchInsert: %v", err)
	}

	recent, err := repo.ListRecent(ctx, projA, "", base.Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(recent) != 3 || recent[0].ID != rows[0].ID || recent[1].ID != rows[1].ID || recent[2].ID != rows[2].ID {
		t.Fatalf("ListRecent (project A, in window, newest first) = %v", evalIDs(recent))
	}
	got := recent[0]
	if got.ProjectID != full.ProjectID || got.TenantID != "t1" || got.ChunkID != chunk || got.RequestRole != "researcher" ||
		got.RequestPurpose != "support" || got.RequestOperator != "op" || got.TraceID != "trace-1" ||
		got.Decision != memoryfirewall.DecisionBlockExpired || got.PolicyDigest != digest || got.ReasonDetail != "expired" ||
		!sameInstant(got.EvaluatedAt, full.EvaluatedAt) {
		t.Fatalf("round trip = %+v, want %+v", got, full)
	}
	if blank := recent[2]; blank.TenantID != "" || blank.TraceID != "" || blank.PolicyDigest != "" || blank.ReasonDetail != "" {
		t.Fatalf("blank optional fields must read back blank: %+v", blank)
	}

	blocks, err := repo.ListRecent(ctx, projA, string(memoryfirewall.DecisionBlockExpired), base.Add(-time.Hour), 50)
	if err != nil || len(blocks) != 1 || blocks[0].ID != full.ID {
		t.Fatalf("ListRecent decision filter = %v, %v", evalIDs(blocks), err)
	}
	one, err := repo.ListRecent(ctx, projA, "", base.Add(-time.Hour), 1)
	if err != nil || len(one) != 1 || one[0].ID != full.ID {
		t.Fatalf("ListRecent limit 1 = %v, %v", evalIDs(one), err)
	}

	byDigest, err := repo.ListByDigest(ctx, digest, 50)
	if err != nil || len(byDigest) != 3 || byDigest[0].ID != rows[4].ID || byDigest[1].ID != rows[0].ID || byDigest[2].ID != rows[1].ID {
		t.Fatalf("ListByDigest = %v, %v", evalIDs(byDigest), err)
	}
	byChunk, err := repo.ListByChunk(ctx, chunk, 50)
	if err != nil || len(byChunk) != 3 || byChunk[0].ID != rows[4].ID || byChunk[2].ID != rows[1].ID {
		t.Fatalf("ListByChunk = %v, %v", evalIDs(byChunk), err)
	}
	if none, err := repo.ListByChunk(ctx, uniqueID("chunk-none"), 50); err != nil || len(none) != 0 {
		t.Fatalf("ListByChunk(unknown) = %v, %v", evalIDs(none), err)
	}
}

func evalIDs(rows []memoryfirewall.EvaluationRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
