//go:build integration

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/postgres"
)

// GitHub #900, GDPR design §4.6.1: re-running an incomplete erasure replaced
// the first run's stored report, so the record of the rows the first run
// deleted was lost (their links went with the rows). Found on the Postgres lane
// by the #478 erase-twice test; `vornikctl subject erase` refuses SQLite, so this
// drives the command's own steps (buildErasurePlan, newSubjectExecutor, Execute,
// writeErasureReport) there.

// rerunErase runs the command's erase steps once, as runSubjectErase does after
// the operator confirms.
func rerunErase(ctx context.Context, t *testing.T, db *sql.DB, repo *postgres.DataSubjectRepository, reqID string) *datasubject.ErasureResult {
	t.Helper()
	req, err := repo.GetRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildErasurePlan(ctx, repo, req)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ex, err := newSubjectExecutor(&erasureDeps{Repo: repo, DB: db, Cfg: &config.Config{}, Root: t.TempDir()}, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, execErr := ex.Execute(ctx, plan)
	if res == nil {
		t.Fatalf("execute returned no result: %v", execErr)
	}
	if err := writeErasureReport(ctx, plan, res, &req, repo); err != nil {
		t.Fatalf("writeErasureReport: %v", err)
	}
	return res
}

// seedRerunSubject creates a subject with a verified Art 17(1)(c) request, an
// exclusive chat turn found by the authenticated binder, and a shared memory
// chunk. With no rewrite model configured the chunk is deferred, so every run
// is incomplete.
func seedRerunSubject(ctx context.Context, t *testing.T, db *sql.DB, repo *postgres.DataSubjectRepository) (chat, chunk, reqID string) {
	t.Helper()
	n := time.Now().UnixNano()
	subj := fmt.Sprintf("ds-rerun900-%d", n)
	chat = fmt.Sprintf("chat-rerun900-%d", n)
	chunk = fmt.Sprintf("chunk-rerun900-%d", n)
	reqID = fmt.Sprintf("req-rerun900-%d", n)
	const proj = "erase-rerun-900"
	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: subj, DisplayName: "Rita Rerun"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO chat_audit_log (id, project_id, user_message)
		VALUES ($1, $2, 'Rita Rerun: my number is 900')`, chat, proj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM chat_audit_log WHERE id = $1`, chat) })
	if err := repo.AddLink(ctx, subj, datasubject.Link{Table: datasubject.TableChatAuditLog, RowID: chat,
		ProjectID: proj, Source: datasubject.SourceAuthenticated, Confidence: datasubject.ConfidenceCertain,
		Exclusivity: datasubject.ExclusiveRow}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash)
		VALUES ($1, $2, 'integration-test', $3, md5($3))`, chunk, proj, "Rita Rerun and Bob met on "+chunk); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM project_memory_chunks WHERE id = $1`, chunk) })
	if err := repo.AddLink(ctx, subj, datasubject.Link{Table: datasubject.TableProjectMemoryChunks, RowID: chunk,
		ProjectID: proj, Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain,
		Exclusivity: datasubject.SharedRow}); err != nil {
		t.Fatal(err)
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
	return chat, chunk, reqID
}

// storedRun is the part of a stored erasure report this test reads.
type storedRun struct {
	Run  int `json:"run"`
	Plan struct {
		Methods []string `json:"identification_methods"`
	} `json:"plan"`
	Result struct {
		Deleted []struct {
			RowID string `json:"row_id"`
		} `json:"deleted"`
		Deferred []struct {
			RowID string `json:"row_id"`
		} `json:"deferred"`
	} `json:"result"`
}

func TestSubjectErase_RerunKeepsEveryRunsReportOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := dbcovOpen(t)
	repo := postgres.NewDataSubjectRepository(db)
	chat, chunk, reqID := seedRerunSubject(ctx, t, db, repo)

	if res := rerunErase(ctx, t, db, repo, reqID); res.RowsDeleted != 1 || len(res.Deferred) != 1 {
		t.Fatalf("first run = %+v, want the chat turn deleted and the chunk deferred", res)
	}
	if res := rerunErase(ctx, t, db, repo, reqID); res.RowsDeleted != 0 || len(res.Deferred) != 1 {
		t.Fatalf("second run = %+v, want nothing deleted and the chunk deferred again", res)
	}

	blob, hash, err := repo.GetRequestReport(ctx, reqID)
	if err != nil || hash == "" {
		t.Fatalf("stored report: err=%v hash=%q", err, hash)
	}
	var doc struct {
		storedRun
		Earlier []storedRun `json:"earlier_runs"`
	}
	if err := json.Unmarshal([]byte(blob), &doc); err != nil {
		t.Fatalf("stored report is not JSON: %v", err)
	}

	// The top level is the latest run: what the escalation pass reads.
	if doc.Run != 2 || len(doc.Result.Deleted) != 0 ||
		len(doc.Result.Deferred) != 1 || doc.Result.Deferred[0].RowID != chunk {
		t.Fatalf("top level = run %d, deleted %v, deferred %v; want run 2 deferring only the chunk",
			doc.Run, doc.Result.Deleted, doc.Result.Deferred)
	}
	// The first run is still on record: the row it deleted and how it was found.
	if len(doc.Earlier) != 1 {
		t.Fatalf("earlier_runs has %d entries, want the first run (report: %s)", len(doc.Earlier), blob)
	}
	first := doc.Earlier[0]
	if first.Run != 1 || len(first.Result.Deleted) != 1 || first.Result.Deleted[0].RowID != chat {
		t.Fatalf("earlier_runs[0] = run %d deleting %v, want run 1 deleting the chat turn %s",
			first.Run, first.Result.Deleted, chat)
	}
	found := false
	for _, m := range first.Plan.Methods {
		found = found || m == string(datasubject.SourceAuthenticated)
	}
	if !found {
		t.Fatalf("earlier_runs[0] identification methods = %v, want %q", first.Plan.Methods, datasubject.SourceAuthenticated)
	}
	if rowExists(t, db, "chat_audit_log", chat) {
		t.Fatal("the chat turn the first run erased is back")
	}
}
