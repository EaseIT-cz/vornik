//go:build integration

package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/postgres"
)

// GitHub #478, GDPR design §7 "idempotence" (§4.20.1 item 25): erasing a whole
// request twice. Observed where an operator meets it, `vornikctl subject erase`,
// which is Postgres-only (requirePostgresSubjectVerb), so this drives the
// command's own steps (buildErasurePlan, newSubjectExecutor, Execute,
// writeErasureReport) on the Postgres lane.
//
// The two ways a second run happens:
//
//  1. The first run was INCOMPLETE. The command leaves the request verified and
//     tells the operator it "still shows as live", so re-running is the
//     documented next step. The re-run must succeed: nothing the first run
//     deleted comes back or fails as missing, the deferred row is deferred
//     again, and the request stays live.
//  2. The first run was COMPLETE and actioned the request. A second run is
//     refused at the identity gate and changes nothing: not the ledger state,
//     not the stored report.

// eraseOnce runs the command's erase steps once, as runSubjectErase does after
// the operator confirms, and returns the reloaded request.
func eraseOnce(ctx context.Context, t *testing.T, db *sql.DB, repo *postgres.DataSubjectRepository, reqID string) (*datasubject.ErasureResult, datasubject.Request, error) {
	t.Helper()
	req, err := repo.GetRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildErasurePlan(ctx, repo, req)
	if err != nil {
		return nil, req, err
	}
	ex, err := newSubjectExecutor(&erasureDeps{Repo: repo, DB: db, Cfg: &config.Config{}, Root: t.TempDir()}, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, execErr := ex.Execute(ctx, plan)
	if res != nil {
		if err := writeErasureReport(ctx, plan, res, &req, repo); err != nil {
			t.Fatalf("writeErasureReport: %v", err)
		}
	}
	after, err := repo.GetRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	return res, after, execErr
}

// seedEraseTwiceSubject creates a subject with a verified Art 17(1)(c) request,
// an exclusive chat turn and, when shared is set, a shared memory chunk. With no
// rewrite model configured the shared chunk is deferred, so the erasure is
// incomplete.
func seedEraseTwiceSubject(ctx context.Context, t *testing.T, db *sql.DB, repo *postgres.DataSubjectRepository, tag string, shared bool) (subj, chat, chunk, reqID string) {
	t.Helper()
	n := time.Now().UnixNano()
	subj = fmt.Sprintf("ds-twice-%s-%d", tag, n)
	chat = fmt.Sprintf("chat-twice-%s-%d", tag, n)
	chunk = fmt.Sprintf("chunk-twice-%s-%d", tag, n)
	reqID = fmt.Sprintf("req-twice-%s-%d", tag, n)
	const proj = "erase-twice-478"
	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: subj, DisplayName: "Anna Twice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO chat_audit_log (id, project_id, user_message)
		VALUES ($1, $2, 'Anna Twice: my number is 777')`, chat, proj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM chat_audit_log WHERE id = $1`, chat) })
	if err := repo.AddLink(ctx, subj, datasubject.Link{Table: datasubject.TableChatAuditLog, RowID: chat,
		ProjectID: proj, Source: datasubject.SourceAuthenticated, Confidence: datasubject.ConfidenceCertain,
		Exclusivity: datasubject.ExclusiveRow}); err != nil {
		t.Fatal(err)
	}
	if shared {
		if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash)
			VALUES ($1, $2, 'integration-test', $3, md5($3))`, chunk, proj,
			"Anna Twice and Bob met on "+chunk); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM project_memory_chunks WHERE id = $1`, chunk) })
		if err := repo.AddLink(ctx, subj, datasubject.Link{Table: datasubject.TableProjectMemoryChunks, RowID: chunk,
			ProjectID: proj, Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain,
			Exclusivity: datasubject.SharedRow}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	req := datasubject.Request{ID: reqID, SubjectID: subj, Kind: datasubject.RequestErasure,
		State: datasubject.StateOpen, OpenedAt: now, ErasureGround: datasubject.GroundObjection}
	if err := repo.CreateRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := req.Verify("operator", "test", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	return subj, chat, chunk, reqID
}

func TestSubjectErase_IncompleteRequestErasedTwiceOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := dbcovOpen(t)
	repo := postgres.NewDataSubjectRepository(db)
	_, chat, chunk, reqID := seedEraseTwiceSubject(ctx, t, db, repo, "incomplete", true)

	res1, req1, err := eraseOnce(ctx, t, db, repo, reqID)
	if err != nil {
		t.Fatalf("first run: %v (%+v)", err, res1)
	}
	if res1.RowsDeleted != 1 || len(res1.Deferred) != 1 || len(res1.Failed) != 0 || res1.Complete() {
		t.Fatalf("first run result = %+v, want one row deleted and the shared chunk deferred", res1)
	}
	if req1.State != datasubject.StateVerified {
		t.Fatalf("after an incomplete erasure the request is %q, want it left verified (live)", req1.State)
	}

	res2, req2, err := eraseOnce(ctx, t, db, repo, reqID)
	if err != nil {
		t.Fatalf("second run failed: %v (%+v)", err, res2)
	}
	if len(res2.Failed) != 0 || res2.RowsDeleted != 0 || len(res2.Deferred) != 1 ||
		res2.Deferred[0].RowID != chunk {
		t.Fatalf("second run result = %+v, want no failure, nothing re-deleted, the chunk deferred again", res2)
	}
	if req2.State != datasubject.StateVerified {
		t.Fatalf("after the second incomplete run the request is %q, want verified", req2.State)
	}
	if rowExists(t, db, "chat_audit_log", chat) {
		t.Fatal("the chat turn the first run erased is back")
	}
	if !rowExists(t, db, "project_memory_chunks", chunk) {
		t.Fatal("the deferred shared chunk was removed although no run could redact it")
	}
	report, hash, err := repo.GetRequestReport(ctx, reqID)
	if err != nil || hash == "" || !strings.Contains(report, chunk) {
		t.Fatalf("stored report after the second run: err=%v hash=%q", err, hash)
	}
	// Not asserted, because it does not hold (found 2026-11-24, GDPR design §7,
	// idempotence bullet): the second run's report REPLACES the first in
	// report_json, so after it the stored report no longer names the chat turn
	// the first run deleted, and that turn's link went with the row (GitHub #900). The
	// "what was erased" record of §4.6 then covers the last run only.
}

func TestSubjectErase_CompleteRequestErasedTwiceOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := dbcovOpen(t)
	repo := postgres.NewDataSubjectRepository(db)
	_, chat, _, reqID := seedEraseTwiceSubject(ctx, t, db, repo, "complete", false)

	res1, req1, err := eraseOnce(ctx, t, db, repo, reqID)
	if err != nil || !res1.Complete() || res1.RowsDeleted != 1 {
		t.Fatalf("first run: err=%v result=%+v", err, res1)
	}
	if req1.State != datasubject.StateActioned || req1.ReportHash == "" {
		t.Fatalf("after a complete erasure the request is %q (hash %q), want actioned", req1.State, req1.ReportHash)
	}
	report1, hash1, err := repo.GetRequestReport(ctx, reqID)
	if err != nil || !strings.Contains(report1, chat) {
		t.Fatalf("first report: err=%v", err)
	}

	res2, req2, err := eraseOnce(ctx, t, db, repo, reqID)
	if !errors.Is(err, datasubject.ErrNotVerified) || res2 != nil {
		t.Fatalf("second run on an actioned request: err=%v result=%+v, want ErrNotVerified and no execution", err, res2)
	}
	if req2.State != datasubject.StateActioned || req2.ReportHash != req1.ReportHash {
		t.Fatalf("the refused second run changed the ledger: %q %q -> %q %q",
			req1.State, req1.ReportHash, req2.State, req2.ReportHash)
	}
	report2, hash2, err := repo.GetRequestReport(ctx, reqID)
	if err != nil || hash2 != hash1 || report2 != report1 {
		t.Fatalf("the refused second run changed the stored report (err=%v, hash %q -> %q)", err, hash1, hash2)
	}
	if rowExists(t, db, "chat_audit_log", chat) {
		t.Fatal("the erased chat turn is back")
	}
}
