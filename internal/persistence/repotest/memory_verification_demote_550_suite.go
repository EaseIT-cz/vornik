package repotest

// GitHub #550 D2 and D5 (design 22, "Amendment 2026-11-15 (#550 D1, D2,
// D5)"): chunks served verified on their own producer's stamp, demoted by an
// audited, recoverable event. Two classes, decided in SQL, on both drivers:
//
//   - companion self-certified (D5): producer and validator both companion:,
//     demoted with confidence capped at 0.3 (the #435 rule);
//   - role stamp of unknown origin (D2): no artifact, no task, and the
//     role-of-record filter reverify-delegated uses, demoted with confidence
//     unchanged.

import (
	"context"
	"sort"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func mvConfidence(t *testing.T, h MemoryVerificationHarness, id string, want float64) {
	t.Helper()
	got, err := h.ReadConfidence(context.Background(), id)
	if err != nil {
		t.Fatalf("read confidence %s: %v", id, err)
	}
	if d := got - want; d > 1e-6 || d < -1e-6 {
		t.Fatalf("chunk %s: confidence=%v, want %v", id, got, want)
	}
}

type demoteFixture struct {
	project                    string
	a1, a2, b1                 string // the selected chunks
	keepProv, keepArtifactRole string // left alone
	keepChat, keepOperator     string
	keepSuperseded, keepTask   string
	keepUnverifiedCompanion    string
}

func seedDemoteFixture(t *testing.T, h MemoryVerificationHarness) demoteFixture {
	t.Helper()
	f := demoteFixture{project: uniqueID("proj")}
	p := f.project
	f.a1, f.a2, f.b1 = uniqueID("c"), uniqueID("c"), uniqueID("c")
	f.keepProv, f.keepArtifactRole = uniqueID("c"), uniqueID("c")
	f.keepChat, f.keepOperator = uniqueID("c"), uniqueID("c")
	f.keepSuperseded, f.keepTask = uniqueID("c"), uniqueID("c")
	f.keepUnverifiedCompanion = uniqueID("c")
	for _, c := range []VerificationSeedChunk{
		// Class A: a companion stamped its own deposit.
		{ID: f.a1, ProjectID: p, ArtifactID: "art-a1-" + p, ProducerRole: "companion:claude-code", ValidatorRole: "companion:claude-code", Status: "verified", Confidence: 0.9},
		{ID: f.a2, ProjectID: p, ProducerRole: "companion:codex", ValidatorRole: "companion:codex", Status: "verified", Confidence: 0.1},
		// Class B: a role stamp with no artifact and no task.
		{ID: f.b1, ProjectID: p, ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified", Confidence: 0.8, SourceName: "review-20260628-d52b.md"},
		// Left alone.
		{ID: f.keepProv, ProjectID: p, ArtifactID: "art-prov-" + p, ProducerRole: "companion:claude-code", ValidatorRole: "provenance:origin/main", Status: "verified", Confidence: 0.9},
		{ID: f.keepArtifactRole, ProjectID: p, ArtifactID: "art-role-" + p, ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified", Confidence: 0.8},
		{ID: f.keepChat, ProjectID: p, ProducerRole: "chat:operator", ValidatorRole: "chat:operator", Status: "verified", Confidence: 0.8},
		{ID: f.keepOperator, ProjectID: p, ProducerRole: "operator_correction", ValidatorRole: "operator_correction", Status: "verified", Confidence: 1},
		{ID: f.keepSuperseded, ProjectID: p, ProducerRole: "companion:claude-code", ValidatorRole: "companion:claude-code", Status: "superseded", Confidence: 0.9},
		{ID: f.keepTask, ProjectID: p, TaskID: mvTask(t, h, p, persistence.TaskCreationSourceUser, ""), ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified", Confidence: 0.8},
		{ID: f.keepUnverifiedCompanion, ProjectID: p, ProducerRole: "companion:claude-code", Status: "unverified", Confidence: 0.9},
	} {
		mvChunk(t, h, c)
	}
	return f
}

func (f demoteFixture) untouched(t *testing.T, h MemoryVerificationHarness) {
	t.Helper()
	mvState(t, h, f.keepProv, "verified", "provenance:origin/main")
	mvState(t, h, f.keepArtifactRole, "verified", "reviewer")
	mvState(t, h, f.keepChat, "verified", "chat:operator")
	mvState(t, h, f.keepOperator, "verified", "operator_correction")
	mvState(t, h, f.keepSuperseded, "superseded", "companion:claude-code")
	mvState(t, h, f.keepTask, "verified", "reviewer")
	mvState(t, h, f.keepUnverifiedCompanion, "unverified", "")
	mvConfidence(t, h, f.keepProv, 0.9)
	mvConfidence(t, h, f.keepSuperseded, 0.9)
}

func surveyClass(s *persistence.SelfCertifiedSurvey, class string) persistence.SelfCertifiedCount {
	for _, c := range s.Classes {
		if c.Class == class {
			return c
		}
	}
	return persistence.SelfCertifiedCount{Class: class}
}

// mvCaseSurveySelectsEachClassAndNothingElse: the dry run's numbers.
func mvCaseSurveySelectsEachClassAndNothingElse(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	f := seedDemoteFixture(t, h)
	s, err := h.Repo.SurveySelfCertified(ctx, f.project)
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	// Verified chunks in the project: a1, a2, b1 and the five verified keeps.
	if s.VerifiedChunks != 8 {
		t.Fatalf("denominator = %d, want 8", s.VerifiedChunks)
	}
	a := surveyClass(s, persistence.SelfCertifiedCompanion)
	b := surveyClass(s, persistence.SelfCertifiedUnknownOrigin)
	if a.Chunks != 2 || b.Chunks != 1 {
		t.Fatalf("class counts: companion=%d unknown_origin=%d, want 2 and 1", a.Chunks, b.Chunks)
	}
	gotA := append([]string(nil), a.Sample...)
	sort.Strings(gotA)
	wantA := []string{f.a1, f.a2}
	sort.Strings(wantA)
	if len(gotA) != 2 || gotA[0] != wantA[0] || gotA[1] != wantA[1] {
		t.Fatalf("class A sample = %v, want %v", gotA, wantA)
	}
	if len(b.Sample) != 1 || b.Sample[0] != f.b1 {
		t.Fatalf("class B sample = %v, want [%s]", b.Sample, f.b1)
	}
	// The survey changes nothing.
	mvState(t, h, f.a1, "verified", "companion:claude-code")
	mvState(t, h, f.b1, "verified", "reviewer")
	f.untouched(t, h)
}

// mvCaseDemoteChangesTheClassesAndRestoresExactly: the apply and the
// restore, confidence included.
func mvCaseDemoteChangesTheClassesAndRestoresExactly(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	f := seedDemoteFixture(t, h)
	if _, err := h.Repo.DemoteSelfCertified(ctx, &persistence.MemoryVerificationEvent{
		ProjectID: f.project, Kind: persistence.MemoryVerificationKindDemote, Actor: "operator"}, 3); err == nil {
		t.Fatal("a demote without a reason was accepted")
	}
	// The race the Skipped count exists for (review-20261115-723f M5): a
	// candidate the dry run counted is superseded before the apply.
	race := uniqueID("c")
	mvChunk(t, h, VerificationSeedChunk{ID: race, ProjectID: f.project, ProducerRole: "companion:claude-code",
		ValidatorRole: "companion:claude-code", Status: "verified", Confidence: 0.9})
	s0, err := h.Repo.SurveySelfCertified(ctx, f.project)
	if err != nil {
		t.Fatal(err)
	}
	counted := surveyClass(s0, persistence.SelfCertifiedCompanion).Chunks + surveyClass(s0, persistence.SelfCertifiedUnknownOrigin).Chunks
	if counted != 4 {
		t.Fatalf("dry run counted %d candidates, want 4", counted)
	}
	if err := h.SetChunk(ctx, race, "superseded", "companion:claude-code"); err != nil {
		t.Fatal(err)
	}
	ev := &persistence.MemoryVerificationEvent{ProjectID: f.project, Kind: persistence.MemoryVerificationKindDemote,
		Actor: "operator", Reason: "GitHub #550", Examined: s0.VerifiedChunks, Unchanged: s0.VerifiedChunks - counted}
	got, err := h.Repo.DemoteSelfCertified(ctx, ev, counted)
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if got.Deverified != 3 || got.Verified != 0 || got.Skipped != 1 || got.Kind != "demote" {
		t.Fatalf("demote event: %+v", got)
	}
	mvState(t, h, f.a1, "unverified", "")
	mvState(t, h, f.a2, "unverified", "")
	mvState(t, h, f.b1, "unverified", "")
	mvConfidence(t, h, f.a1, 0.3) // capped
	mvConfidence(t, h, f.a2, 0.1) // already below the cap
	mvConfidence(t, h, f.b1, 0.8) // class B keeps its confidence
	mvState(t, h, race, "superseded", "companion:claude-code")
	mvConfidence(t, h, race, 0.9)
	f.untouched(t, h)
	stored, err := h.Repo.GetVerificationEvent(ctx, got.ID)
	if err != nil || stored == nil || stored.Deverified != 3 || stored.Skipped != 1 || stored.Examined != 9 || stored.Kind != "demote" {
		t.Fatalf("stored event: %+v %v", stored, err)
	}
	if n, err := h.CountBackup(ctx, got.ID); err != nil || n != 3 {
		t.Fatalf("backup rows = %d (%v), want 3", n, err)
	}
	// A second run finds nothing: the classes are now empty.
	s, err := h.Repo.SurveySelfCertified(ctx, f.project)
	if err != nil || surveyClass(s, persistence.SelfCertifiedCompanion).Chunks != 0 ||
		surveyClass(s, persistence.SelfCertifiedUnknownOrigin).Chunks != 0 {
		t.Fatalf("survey after demote: %+v %v", s, err)
	}

	// a2's confidence alone moves after the demote (F1): the restore leaves
	// it, status and validator included.
	if err := h.SetConfidence(ctx, f.a2, 0.2); err != nil {
		t.Fatal(err)
	}
	rs, err := h.Repo.RestoreVerificationEvent(ctx, got.ID, "operator", "undo #550")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if rs.Examined != 3 || rs.Verified != 2 || rs.Skipped != 1 {
		t.Fatalf("restore event: %+v", rs)
	}
	mvState(t, h, f.a1, "verified", "companion:claude-code")
	mvState(t, h, f.b1, "verified", "reviewer")
	mvConfidence(t, h, f.a1, 0.9)
	mvConfidence(t, h, f.b1, 0.8)
	mvState(t, h, f.a2, "unverified", "")
	mvConfidence(t, h, f.a2, 0.2)

	// The restore is itself restorable, confidence included.
	if _, err := h.Repo.RestoreVerificationEvent(ctx, rs.ID, "operator", "redo"); err != nil {
		t.Fatalf("restore of the restore: %v", err)
	}
	mvState(t, h, f.a1, "unverified", "")
	mvConfidence(t, h, f.a1, 0.3)
}

// mvCaseRestoreOfAnOlderEventLeavesConfidence: an event with no recorded
// confidence (every event before #550) restores status and validator only.
func mvCaseRestoreOfAnOlderEventLeavesConfidence(t *testing.T, h MemoryVerificationHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	art, c1 := "old-"+project, uniqueID("c")
	mvChunk(t, h, VerificationSeedChunk{ID: c1, ProjectID: project, ArtifactID: art, ProducerRole: "reviewer", ValidatorRole: "reviewer", Status: "verified", Confidence: 0.7})
	ev := &persistence.MemoryVerificationEvent{ProjectID: project, Kind: "backfill", Actor: "operator", Reason: "x"}
	if err := h.Repo.ApplyVerificationChange(ctx, ev, []persistence.ArtifactVerificationChange{{ArtifactID: art, NewStatus: "unverified"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.SetConfidence(ctx, c1, 0.4); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Repo.RestoreVerificationEvent(ctx, ev.ID, "operator", "undo"); err != nil {
		t.Fatal(err)
	}
	mvState(t, h, c1, "verified", "reviewer")
	mvConfidence(t, h, c1, 0.4)
}
