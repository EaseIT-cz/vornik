package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #115 status audit (2026-10-21, GDPR design §4.4 and §4.20): the Art 15
// report the subject receives must list the identifiers the deployment holds
// about them. runSubjectExport fetched them and attached them to the export, but
// the field was tagged json:"-", so they were dropped from the artefact the
// subject receives and from its pinned hash. Found at: audit (the user-observable
// level is the `vornikctl subject export` artefact); this test reads that
// artefact, built on a real SQLite store.
func TestSubjectExport_ReportListsTheIdentifiersHeld(t *testing.T) {
	db := sqlitetest.File(t, "export-identifiers.db")
	repo := sqlite.NewDataSubjectRepository(db.DB)
	ctx := context.Background()

	const subj = "ds_export_ids"
	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: subj, DisplayName: "Jane Example"}); err != nil {
		t.Fatalf("CreateSubject: %v", err)
	}
	if err := repo.AddIdentifier(ctx, subj, datasubject.Identifier{Kind: "email", Value: "jane@example.org",
		Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceProbable}); err != nil {
		t.Fatalf("AddIdentifier: %v", err)
	}
	now := time.Now().UTC()
	req := datasubject.Request{ID: "dsr_export_ids", SubjectID: subj, Kind: datasubject.RequestAccess,
		State: datasubject.StateOpen, OpenedAt: now}
	if err := repo.CreateRequest(ctx, req); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if err := req.Verify("operator", "reply from the address on file", now); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	_, ids, blob, hash, err := assembleSubjectExport(ctx, repo, req)
	if err != nil {
		t.Fatalf("assembleSubjectExport: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("the store holds one identifier, got %d", len(ids))
	}
	if !strings.HasPrefix(hash, "sha256:") {
		t.Errorf("the artefact must be pinned by its hash, got %q", hash)
	}

	var got struct {
		Identifiers []struct {
			Kind, Value, Source, Confidence string
		} `json:"identifiers"`
	}
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("the artefact is JSON: %v", err)
	}
	if len(got.Identifiers) != 1 {
		t.Fatalf("the report must list the identifiers held (Art 15(1)); artefact:\n%s", blob)
	}
	id := got.Identifiers[0]
	if id.Kind != "email" || id.Value != "jane@example.org" || id.Source != "operator_asserted" || id.Confidence != "probable" {
		t.Errorf("identifier must carry kind, value, source and confidence, got %+v", id)
	}
}

// GDPR design §7 asks for an Art 15(4) leakage test with a two-subject fixture,
// end to end. The #115 status audit (2026-10-21, §4.20) found it only in parts:
// BuildExport with hand-set exclusivity, and the store suite asserting the
// re-derived exclusivity without building a report. This drives the artefact
// itself: a row a binder called exclusive to A, which B is also linked to, must
// reach A's report as a withheld item, while A's own row arrives whole.
func TestSubjectExport_SharedRowIsWithheldEndToEnd(t *testing.T) {
	db := sqlitetest.File(t, "export-two-subjects.db")
	repo := sqlite.NewDataSubjectRepository(db.DB)
	ctx := context.Background()
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	for _, row := range [][2]string{
		{"op-own", "note about A alone: prefers e-mail"},
		{"op-shared", "A and B met; B disclosed BYSTANDER-SECRET"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO operator_profile (operator_id, notes, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			row[0], row[1], stamp, stamp); err != nil {
			t.Fatalf("seed %s: %v", row[0], err)
		}
	}
	link := func(subj, row string) {
		t.Helper()
		if err := repo.AddLink(ctx, subj, datasubject.Link{Table: datasubject.TableOperatorProfile, RowID: row,
			Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceProbable,
			Exclusivity: datasubject.ExclusiveRow}); err != nil {
			t.Fatalf("AddLink %s %s: %v", subj, row, err)
		}
	}
	for _, s := range []string{"ds_a", "ds_b"} {
		if err := repo.CreateSubject(ctx, datasubject.Subject{ID: s, DisplayName: s}); err != nil {
			t.Fatalf("CreateSubject %s: %v", s, err)
		}
	}
	link("ds_a", "op-own")
	link("ds_a", "op-shared") // the binder's guess: exclusive
	link("ds_b", "op-shared") // the observed fact: B is on it too

	now := time.Now().UTC()
	req := datasubject.Request{ID: "dsr_two", SubjectID: "ds_a", Kind: datasubject.RequestAccess,
		State: datasubject.StateOpen, OpenedAt: now}
	if err := repo.CreateRequest(ctx, req); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if err := req.Verify("operator", "authenticated session", now); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	exp, _, blob, _, err := assembleSubjectExport(ctx, repo, req)
	if err != nil {
		t.Fatalf("assembleSubjectExport: %v", err)
	}
	if strings.Contains(string(blob), "BYSTANDER-SECRET") {
		t.Fatalf("Art 15(4): A's report discloses a record that also concerns B:\n%s", blob)
	}
	if !strings.Contains(string(blob), "prefers e-mail") {
		t.Errorf("A's own record must be produced whole:\n%s", blob)
	}
	var withheld int
	for _, it := range exp.Items {
		if it.RowID == "op-shared" {
			if it.Withheld == "" || it.Content != "" {
				t.Errorf("the shared row must be reported as withheld, got %+v", it)
			}
			withheld++
		}
	}
	if withheld != 1 {
		t.Errorf("the subject must be told the shared record exists (one withheld item), got %d", withheld)
	}
}
