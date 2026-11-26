//go:build integration

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence/postgres"
)

// GitHub #901, GDPR design §4.16.8: re-running an incomplete Art 16
// rectification replaced the first run's stored report, and with it the only
// record of the inaccurate originals the first run corrected (§4.16.2 promises
// they are retained). `vornikctl subject rectify` refuses SQLite, so this runs
// on the Postgres lane through rectifyRequest, the command's own path after the
// database is open.

// seedRectifySubject creates a subject with a verified rectification request and
// two linked memory chunks that both contain the inaccuracy.
func seedRectifySubject(ctx context.Context, t *testing.T, db *sql.DB, repo *postgres.DataSubjectRepository) (first, second, reqID string) {
	t.Helper()
	n := time.Now().UnixNano()
	subj := fmt.Sprintf("ds-rerun901-%d", n)
	first = fmt.Sprintf("chunk-rerun901-a-%d", n)
	second = fmt.Sprintf("chunk-rerun901-b-%d", n)
	reqID = fmt.Sprintf("req-rerun901-%d", n)
	const proj = "rectify-rerun-901"
	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: subj, DisplayName: "Rita Rectify"}); err != nil {
		t.Fatal(err)
	}
	for id, content := range map[string]string{
		first:  "Rita Rectify works at Acmee, " + first,
		second: "Rita Rectify joined Acmee in 2019, " + second,
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash)
			VALUES ($1, $2, 'integration-test', $3, $4)`, id, proj, content, memory.ContentHash(content)); err != nil {
			t.Fatal(err)
		}
		id := id
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM project_memory_chunks WHERE id = $1`, id) })
		if err := repo.AddLink(ctx, subj, datasubject.Link{Table: datasubject.TableProjectMemoryChunks, RowID: id,
			ProjectID: proj, Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain,
			Exclusivity: datasubject.ExclusiveRow}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	req := datasubject.Request{ID: reqID, SubjectID: subj, Kind: datasubject.RequestRectification,
		State: datasubject.StateOpen, OpenedAt: now}
	if err := repo.CreateRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := req.Verify("operator", "test", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	return first, second, reqID
}

// storedRectifyRun is the part of a stored rectification report this test reads.
type storedRectifyRun struct {
	Run    int `json:"run"`
	Result struct {
		Corrected []struct {
			RowID  string `json:"row_id"`
			Before string `json:"before"`
		} `json:"corrected"`
		Deferred []struct {
			RowID string `json:"row_id"`
		} `json:"deferred"`
	} `json:"result"`
}

func TestSubjectRectify_RerunKeepsEveryRunsReportOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := dbcovOpen(t)
	repo := postgres.NewDataSubjectRepository(db)
	first, second, reqID := seedRectifySubject(ctx, t, db, repo)
	correction := datasubject.Correction{Incorrect: "Acmee", Correct: "Acme"}
	subjectRights6Out = ""

	run := func(approve func(datasubject.RectificationProposal) (bool, error)) {
		t.Helper()
		req, err := repo.GetRequest(ctx, reqID)
		if err != nil {
			t.Fatal(err)
		}
		if err := rectifyRequest(ctx, repo, db, req, correction, approve); err != nil {
			t.Fatalf("rectifyRequest: %v", err)
		}
	}
	// Run 1: the operator approves the first chunk and declines the second, so
	// the run is incomplete and the request stays verified.
	run(func(p datasubject.RectificationProposal) (bool, error) { return p.RowID == first, nil })
	// Run 2: the second chunk is approved; the first no longer contains the
	// inaccuracy, so this run's plan does not list it.
	run(func(datasubject.RectificationProposal) (bool, error) { return true, nil })

	blob, hash, err := repo.GetRequestReport(ctx, reqID)
	if err != nil || hash == "" {
		t.Fatalf("stored report: err=%v hash=%q", err, hash)
	}
	var doc struct {
		storedRectifyRun
		Earlier []storedRectifyRun `json:"earlier_runs"`
	}
	if err := json.Unmarshal([]byte(blob), &doc); err != nil {
		t.Fatalf("stored report is not JSON: %v", err)
	}
	if doc.Run != 2 || len(doc.Result.Corrected) != 1 || doc.Result.Corrected[0].RowID != second {
		t.Fatalf("top level = run %d correcting %v; want run 2 correcting only %s", doc.Run, doc.Result.Corrected, second)
	}
	if len(doc.Earlier) != 1 {
		t.Fatalf("earlier_runs has %d entries, want the first run (report: %s)", len(doc.Earlier), blob)
	}
	prev := doc.Earlier[0]
	if prev.Run != 1 || len(prev.Result.Corrected) != 1 || prev.Result.Corrected[0].RowID != first ||
		!strings.Contains(prev.Result.Corrected[0].Before, "Acmee") {
		t.Fatalf("earlier_runs[0] = run %d correcting %+v; want run 1 keeping %s's inaccurate original",
			prev.Run, prev.Result.Corrected, first)
	}
	if len(prev.Result.Deferred) != 1 || prev.Result.Deferred[0].RowID != second {
		t.Fatalf("earlier_runs[0] deferred = %v, want the declined %s", prev.Result.Deferred, second)
	}
	req, err := repo.GetRequest(ctx, reqID)
	if err != nil || req.State != datasubject.StateActioned {
		t.Fatalf("request state = %q (err %v), want actioned after the complete second run", req.State, err)
	}
}
