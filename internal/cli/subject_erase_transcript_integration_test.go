//go:build integration

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/postgres"
)

// GitHub #344, slice 5d-1 (design 2026-07-29-art17-redact-and-reembed §15.14,
// "Integration"). Found at: review (the 5d design); the operator observes it
// as `vornikctl subject erase` and the stored report, so this drives the
// command's own wiring (newSubjectExecutor, newRedactDeps, writeErasureReport)
// on Postgres with no rewrite model configured: a shared transcript must be
// erased without one, and the report must say what was done to each row.
func TestSubjectErase_SharedTranscriptOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := dbcovOpen(t)
	repo := postgres.NewDataSubjectRepository(db)
	n := time.Now().UnixNano()
	id := func(p string) string { return fmt.Sprintf("%s-%d", p, n) }
	proj, anna, bob := id("p344"), id("anna"), id("bob")
	annaTG, chat := "telegram:"+id("4242"), id("group")

	for _, s := range []string{anna, bob} {
		if err := repo.CreateSubject(ctx, datasubject.Subject{ID: s, DisplayName: s}); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{annaTG, "Anna Novak"} {
		if err := repo.AddIdentifier(ctx, anna, datasubject.Identifier{Kind: "channel", Value: v,
			Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain}); err != nil {
			t.Fatal(err)
		}
	}
	own, other, origin := id("own"), id("other"), id("task")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO chat_audit_log (id, ts, chat_id, user_id, project_id, user_message, response)
		VALUES ($1, now() - interval '2 minutes', $2, $3, $4, 'call me, Anna Novak, on 777', 'noted')`, own, chat, annaTG, proj)
	exec(`INSERT INTO chat_audit_log (id, ts, chat_id, user_id, project_id, user_message, response)
		VALUES ($1, now() - interval '1 minute', $2, 'telegram:bob', $3, 'Anna Novak asked me to call', 'ok')`, other, chat, proj)
	exec(`INSERT INTO tasks (id, project_id, status, chat_turn_id, payload, created_at, updated_at)
		VALUES ($1, $2, 'COMPLETED', $3, '{"goal": "remind Anna Novak"}', now(), now())`, origin, proj, own)
	for _, row := range []string{own, other} {
		for _, s := range []string{anna, bob} {
			if err := repo.AddLink(ctx, s, datasubject.Link{Table: datasubject.TableChatAuditLog, RowID: row, ProjectID: proj,
				Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain,
				Exclusivity: datasubject.SharedRow}); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := time.Now().UTC()
	req := datasubject.Request{ID: id("req"), SubjectID: anna, Kind: datasubject.RequestErasure,
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

	plan, err := buildErasurePlan(ctx, repo, req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RedactCount() != 2 {
		t.Fatalf("plan = %+v", plan.Actions)
	}
	oldApply := subjectEraseApply
	subjectEraseApply = true
	defer func() { subjectEraseApply = oldApply }()
	if err := assessResidual(plan, "GDPR Art 17(3)(a): their own recorded speech", "test-operator", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ex, err := newSubjectExecutor(&erasureDeps{Repo: repo, DB: db, Cfg: &config.Config{}, Root: t.TempDir()}, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ex.Execute(ctx, plan)
	if err != nil {
		t.Fatalf("execute: %v (%+v)", err, res)
	}
	if err := writeErasureReport(ctx, plan, res, &req, repo); err != nil {
		t.Fatal(err)
	}

	var userID, msg, otherMsg, payload string
	if err := db.QueryRow(`SELECT user_id, user_message FROM chat_audit_log WHERE id = $1`, own).Scan(&userID, &msg); err != nil {
		t.Fatal(err)
	}
	if userID != "removed-participant-1" || !strings.HasPrefix(msg, "[message removed at the request of a participant on ") {
		t.Fatalf("own turn = %q / %q", userID, msg)
	}
	if err := db.QueryRow(`SELECT user_message FROM chat_audit_log WHERE id = $1`, other).Scan(&otherMsg); err != nil {
		t.Fatal(err)
	}
	if otherMsg != "[person removed] asked me to call [identifiers of a participant were masked]" {
		t.Fatalf("other turn = %q", otherMsg)
	}
	if err := db.QueryRow(`SELECT payload::text FROM tasks WHERE id = $1`, origin).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "Anna") {
		t.Fatalf("payload = %s", payload)
	}

	blob, _, err := repo.GetRequestReport(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Narrative datasubject.SubjectNarrative `json:"subject_narrative"`
		Plan      datasubject.ErasurePlan      `json:"plan"`
		Result    datasubject.ErasureResult    `json:"result"`
	}
	if err := json.Unmarshal([]byte(blob), &report); err != nil {
		t.Fatal(err)
	}
	// #346 Q2: the per-request assessment is in the stored report.
	if a := report.Plan.ResidualAssessment; a == nil || a.By != "test-operator" ||
		report.Narrative.ResidualGround != "GDPR Art 17(3)(a): their own recorded speech" {
		t.Fatalf("residual assessment in the report = %+v / %q", a, report.Narrative.ResidualGround)
	}
	kinds := map[string]int{}
	for _, r := range report.Result.Redacted {
		kinds[r.Kind]++
	}
	if kinds[datasubject.TranscriptTombstoned] != 1 || kinds[datasubject.TranscriptMasked] != 1 ||
		kinds[datasubject.TranscriptPayloadMasked] != 1 || len(report.Result.Deferred) != 0 {
		t.Fatalf("report result = %+v", report.Result)
	}
	outcomes := map[string]bool{}
	for _, e := range report.Narrative.Entries {
		outcomes[e.Outcome] = true
	}
	if !outcomes[datasubject.NarrativeTombstoned] || !outcomes[datasubject.NarrativeMasked] || !outcomes[datasubject.NarrativePayloadMasked] {
		t.Fatalf("narrative = %+v", report.Narrative.Entries)
	}
	got, err := repo.GetRequest(ctx, req.ID)
	if err != nil || got.State != datasubject.StateActioned {
		t.Fatalf("request = %+v, %v", got, err)
	}
}
