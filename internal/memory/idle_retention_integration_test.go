//go:build integration

package memory_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/retention"
)

// Issue #70: exercise real recall and deletion SQL, rather than just expiry math.
func TestIntegrationIdleRetention_RecallAndApprovedDeletion(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("idle70-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE project_id=$1`, project) })
	now := time.Now().UTC()
	created := now.AddDate(0, 0, -400)
	for _, name := range []string{"used", "idle", "long", "forever"} {
		ttl := any(created.AddDate(0, 0, 30))
		if name == "long" {
			ttl = created.AddDate(0, 0, 730)
		}
		if name == "forever" {
			ttl = nil
		}
		_, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks (id,project_id,source_name,content,content_hash,created_at,expires_at,content_class,validation_status,lifecycle_state) VALUES ($1,$2,$3,$4,$5,$6,$7,'companion_note','verified','published')`, project+name, project, name, "memorandum "+name, project+name, created, ttl)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO data_subjects(id,display_name) VALUES($1,'Idle retention regression')`, project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, `DELETE FROM data_subjects WHERE id=$1`, project) })
	if _, err := db.ExecContext(ctx, `INSERT INTO data_subject_links(subject_id,table_name,row_id,project_id,source,confidence,exclusivity) VALUES($1,'project_memory_chunks',$2,$1,'test','high','exclusive')`, project, project+"idle"); err != nil {
		t.Fatal(err)
	}
	repo := memory.NewRepository(db)
	repo.SetApprovalRetentionResolver(func(p string) bool { return p == project })
	searcher := memory.NewSearcher(memory.DefaultConfig(), repo, nil)
	got, err := searcher.Search(ctx, project, "used", 5)
	if err != nil || len(got) != 1 || got[0].ChunkID != project+"used" {
		t.Fatalf("expired note inaccessible pending approval: %v %v", got, err)
	}
	sweeper := retention.New(db, zerolog.Nop())
	requests, err := sweeper.MemoryApprovalRequests(ctx, project, 365, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("only idle finite short-TTL chunk eligible, got %d", len(requests))
	}
	requests[0].Status = "approved"
	requests[0].DecidedByDevice = "test-device"
	// A returned hit resets idle age, so an existing request must not delete it.
	got, err = searcher.Search(ctx, project, "idle", 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("idle recall: %v %v", got, err)
	}
	if _, err = sweeper.ApplyMemoryApproval(ctx, requests[0], 365, now); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1`, project).Scan(&n); err != nil || n != 4 {
		t.Fatalf("stale approval deleted used data: %d %v", n, err)
	}
	// Reset to idle, request a fresh snapshot, then approve precisely that row.
	if _, err = db.ExecContext(ctx, `UPDATE project_memory_chunks SET last_used_at=NULL WHERE id=$1`, project+"idle"); err != nil {
		t.Fatal(err)
	}
	requests, err = sweeper.MemoryApprovalRequests(ctx, project, 365, now)
	if err != nil || len(requests) != 1 {
		t.Fatalf("fresh request: %v %v", requests, err)
	}
	requests[0].Status = "approved"
	requests[0].DecidedByDevice = "test-device"
	if _, err = sweeper.ApplyMemoryApproval(ctx, requests[0], 730, now); err == nil {
		t.Fatal("changed policy accepted")
	}
	for _, mutation := range []struct {
		query          string
		value, restore any
	}{
		{`UPDATE project_memory_chunks SET content_hash=$2 WHERE id=$1`, "changed", project + "idle"},
		{`UPDATE project_memory_chunks SET expires_at=$2 WHERE id=$1`, now.AddDate(0, 0, 800), created.AddDate(0, 0, 30)},
		{`UPDATE project_memory_chunks SET last_used_at=$2 WHERE id=$1`, created, nil},
	} {
		if _, err = db.ExecContext(ctx, mutation.query, project+"idle", mutation.value); err != nil {
			t.Fatal(err)
		}
		if n, err = sweeper.ApplyMemoryApproval(ctx, requests[0], 365, now); err != nil || n != 0 {
			t.Fatalf("changed chunk deleted: %d %v", n, err)
		}
		if _, err = db.ExecContext(ctx, mutation.query, project+"idle", mutation.restore); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM data_subject_links WHERE row_id=$1`, project+"idle").Scan(&n); err != nil || n != 1 {
		t.Fatalf("stale approval removed subject link: %d %v", n, err)
	}
	if n, err = sweeper.ApplyMemoryApproval(ctx, requests[0], 365, now); err != nil || n != 1 {
		t.Fatalf("approved deletion: %d %v", n, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM data_subject_links WHERE row_id=$1`, project+"idle").Scan(&n); err != nil || n != 0 {
		t.Fatalf("approved deletion orphaned subject link: %d %v", n, err)
	}
	if n, err = sweeper.ApplyMemoryApproval(ctx, requests[0], 365, now); err != nil || n != 0 {
		t.Fatalf("replayed deletion: %d %v", n, err)
	}
}

// Issue #70: unanswered/rejected requests never delete; actual phone decisions
// execute the existing shared cleanup and respect the 30-day re-ask interval.
func TestIntegrationIdleRetention_PhoneDecisionAndCooldown(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("idle70-phone-%d", time.Now().UnixNano())
	id := project + "-chunk"
	now := time.Now().UTC()
	created := now.AddDate(0, 0, -400)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE project_id=$1`, project)
		_, _ = db.ExecContext(ctx, `DELETE FROM agent_approval_requests WHERE namespace=$1`, "memory:"+project)
	})
	if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks(id,project_id,source_name,content,content_hash,created_at,expires_at) VALUES($1,$2,'private-note','secretpreview cabbage',$1,$3,$3::timestamptz+INTERVAL '30 days')`, id, project, created); err != nil {
		t.Fatal(err)
	}
	s := retention.New(db, zerolog.Nop())
	// Standalone CLI and daemon both use Sweep: even an explicit age cap cannot
	// override approval. There is deliberately no phone service on this sweep.
	policy := retention.Resolve(project, retention.Policy{MemoryRequireApproval: true, MemoryChunksDays: 1}, retention.Policy{})
	counts, err := s.Sweep(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if counts.MemoryChunks != 0 || counts.MemoryExpired != 0 {
		t.Fatalf("silent deletion: %+v", counts)
	}
	store := postgres.NewApproverDeviceRepository(db)
	pushed := ""
	devices := approverdevice.New(store, approverdevice.WithClock(func() time.Time { return now }), approverdevice.WithNotifier(func(_ context.Context, subject, body string) { pushed = subject + body }))
	devices.RegisterEffect(persistence.ApprovalKindMemoryRetention, func(ctx context.Context, r persistence.AgentApprovalRequestRow) error {
		_, e := s.ApplyMemoryApproval(ctx, r, 365, now)
		return e
	})
	requests, err := s.MemoryApprovalRequests(ctx, project, 0, now)
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests: %v %v", requests, err)
	}
	if err = devices.FileRequestBatch(ctx, requests, "Unused memory needs a deletion decision"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pushed, "secretpreview") {
		t.Fatal("preview leaked into notification")
	}
	if err = devices.Decide(ctx, nil, requests[0].ID, requests[0].RenderedSHA256, true); !errors.Is(err, approverdevice.ErrNoDevice) {
		t.Fatalf("non-phone approval accepted: %v", err)
	}
	if _, err = s.ApplyMemoryApproval(ctx, requests[0], 365, now); err == nil {
		t.Fatal("pending request deleted data")
	}
	suppressed, err := s.MemoryApprovalRequests(ctx, project, 365, now.AddDate(0, 0, 29))
	if err != nil || len(suppressed) != 0 {
		t.Fatalf("unanswered re-asked too soon: %v %v", suppressed, err)
	}
	// Existing generic device gate is used; no second API or self-approval path.
	device := &approverdevice.Device{ID: "idle70-test-phone"}
	if err = devices.Decide(ctx, device, requests[0].ID, requests[0].RenderedSHA256, false); err != nil {
		t.Fatal(err)
	}
	suppressed, err = s.MemoryApprovalRequests(ctx, project, 365, now.AddDate(0, 0, 29))
	if err != nil || len(suppressed) != 0 {
		t.Fatalf("rejected re-asked too soon: %v %v", suppressed, err)
	}
	now = now.AddDate(0, 0, 31)
	requests, err = s.MemoryApprovalRequests(ctx, project, 365, now)
	if err != nil || len(requests) != 1 {
		t.Fatalf("no re-ask after 30 days: %v %v", requests, err)
	}
	if err = devices.FileRequestBatch(ctx, requests, "Unused memory needs a deletion decision"); err != nil {
		t.Fatal(err)
	}
	if err = devices.Decide(ctx, device, requests[0].ID, "wrong-hash", true); !errors.Is(err, approverdevice.ErrNotDecidable) {
		t.Fatalf("mismatched displayed content accepted: %v", err)
	}
	if err = devices.Decide(ctx, device, requests[0].ID, requests[0].RenderedSHA256, true); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_memory_chunks WHERE id=$1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("approved data still present: %d %v", n, err)
	}
}

// A deterministic row-lock interleaving models approval winning after SELECT
// but before the recall's use update. No deleted snippet may escape as a hit.
func TestIntegrationIdleRetention_DeleteWinsRecallRace(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	project := fmt.Sprintf("idle70-race-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM project_memory_chunks WHERE project_id=$1`, project)
	})
	_, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks(id,project_id,source_name,content,content_hash,created_at,expires_at) VALUES($1,$1,'race-note','cabbage memory',$1,NOW()-INTERVAL '400 days',NOW()-INTERVAL '370 days')`, project)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM project_memory_chunks WHERE id=$1 FOR UPDATE`, project).Scan(&id); err != nil {
		t.Fatal(err)
	}
	repo := memory.NewRepository(db)
	repo.SetApprovalRetentionResolver(func(string) bool { return true })
	s := memory.NewSearcher(memory.DefaultConfig(), repo, nil)
	type result struct {
		hits []memory.SearchResult
		err  error
	}
	done := make(chan result, 1)
	go func() { hits, e := s.Search(ctx, project, "cabbage", 5); done <- result{hits, e} }()
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'UPDATE project_memory_chunks SET last_used_at%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("recall never reached the contested use update")
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id=$1`, project); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || len(got.hits) != 0 {
		t.Fatalf("deleted snippet escaped as a returned hit: %v %v", got.hits, got.err)
	}
}
