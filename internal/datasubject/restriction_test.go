package datasubject

import (
	"strings"
	"testing"
	"time"
)

// GDPR increment 6 (GitHub #343), Art 18 restriction — the decision layer.
//
// audit 2026-10-12: Art 18 requests were recorded in the ledger and nothing
// could execute them (DPA §8 limitation 7). These tests pin the rules the
// restriction record must obey before anything enforces it.
//
// see LLD § https://docs.vornik.io §4.16.1

func verifiedRestrictionRequest() Request {
	return Request{
		ID: "dsr_1", SubjectID: "ds_1", Kind: RequestRestriction,
		State: StateVerified, OpenedAt: time.Now().Add(-time.Hour),
		VerifiedBy: "operator", VerifiedHow: "channel round-trip",
	}
}

func TestRestrictionGround_ArticleLetteringMatchesTheRegulation(t *testing.T) {
	want := map[RestrictionGround]string{
		RestrictAccuracyContested:      "Art 18(1)(a)",
		RestrictUnlawfulOpposesErasure: "Art 18(1)(b)",
		RestrictLegalClaims:            "Art 18(1)(c)",
		RestrictObjectionPending:       "Art 18(1)(d)",
	}
	for g, art := range want {
		if err := g.Validate(); err != nil {
			t.Errorf("%s: %v", g, err)
		}
		if g.Article() != art {
			t.Errorf("%s: article %q, want %q", g, g.Article(), art)
		}
		if strings.TrimSpace(g.Label()) == "" {
			t.Errorf("%s: no label", g)
		}
	}
	if err := RestrictionGround("").Validate(); err == nil {
		t.Error("an empty ground must be refused")
	}
	if err := RestrictionGround("because").Validate(); err == nil {
		t.Error("an unknown ground must be refused")
	}
}

func TestNewRestriction_RequiresAVerifiedRestrictionRequest(t *testing.T) {
	now := time.Now()
	req := verifiedRestrictionRequest()

	r, err := NewRestriction(req, RestrictAccuracyContested, "operator", now)
	if err != nil {
		t.Fatalf("NewRestriction: %v", err)
	}
	if !r.Active() || r.SubjectID != "ds_1" || r.RequestID != "dsr_1" || r.Ground != RestrictAccuracyContested {
		t.Fatalf("unexpected restriction %+v", r)
	}
	if r.ID == "" {
		t.Error("a restriction needs an id")
	}

	open := req
	open.State = StateOpen
	if _, err := NewRestriction(open, RestrictAccuracyContested, "operator", now); err == nil {
		t.Error("an UNVERIFIED request must not restrict anything (Art 12(6) gate)")
	}
	wrongKind := req
	wrongKind.Kind = RequestErasure
	if _, err := NewRestriction(wrongKind, RestrictAccuracyContested, "operator", now); err == nil {
		t.Error("only a restriction request may impose a restriction")
	}
	if _, err := NewRestriction(req, "", "operator", now); err == nil {
		t.Error("a restriction needs its Art 18(1) ground")
	}
	if _, err := NewRestriction(req, RestrictLegalClaims, "  ", now); err == nil {
		t.Error("an unattributed restriction must be refused")
	}
}

// Art 18(3): the subject who obtained a restriction must be informed BEFORE it
// is lifted. The software cannot inform them, so the lift refuses without the
// operator's affirmation that they were.
func TestRestrictionLift_RequiresTheSubjectToHaveBeenInformed(t *testing.T) {
	now := time.Now()
	r, err := NewRestriction(verifiedRestrictionRequest(), RestrictAccuracyContested, "operator", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Lift("operator", "accuracy verified", false, "", now); err == nil {
		t.Fatal("a lift without the Art 18(3) affirmation must be refused")
	}
	if !r.Active() {
		t.Fatal("a refused lift must leave the restriction in force")
	}
	if err := r.Lift("operator", "", true, "", now); err == nil {
		t.Error("a lift needs a reason")
	}
	if err := r.Lift("", "accuracy verified", true, "", now); err == nil {
		t.Error("a lift needs an operator")
	}
	if err := r.Lift("operator", "accuracy verified", true, "dsr_2", now); err != nil {
		t.Fatalf("Lift: %v", err)
	}
	if r.Active() || r.LiftedBy != "operator" || r.LiftReason != "accuracy verified" ||
		r.LiftRequestID != "dsr_2" || r.SubjectInformedAt.IsZero() {
		t.Fatalf("lift not recorded: %+v", r)
	}
	if err := r.Lift("operator", "again", true, "", now); err == nil {
		t.Error("lifting an already-lifted restriction must be refused")
	}
}

// The report must publish its DENOMINATOR: every linked row, split into what the
// software withholds and what it does not, with the reason per table. A report
// that listed only the withheld chunks would read as "everything is restricted".
func TestRestrictionReport_SplitsWithheldFromNotWithheldWithReasons(t *testing.T) {
	r, err := NewRestriction(verifiedRestrictionRequest(), RestrictObjectionPending, "operator", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	links := []Link{
		{Table: TableProjectMemoryChunks, RowID: "c1", Exclusivity: ExclusiveRow},
		{Table: TableProjectMemoryChunks, RowID: "c2", Exclusivity: SharedRow},
		{Table: TableProjectMemoryChunks, RowID: "c3", Exclusivity: UnknownExclusivity},
		{Table: TableChatAuditLog, RowID: "a1", Exclusivity: ExclusiveRow},
		{Table: TableKnowledgeEntities, RowID: "e1", Exclusivity: ExclusiveRow},
	}
	rep := BuildRestrictionReport(r, links)

	if rep.RowsExamined != 5 {
		t.Errorf("RowsExamined = %d, want 5 — the denominator is every linked row", rep.RowsExamined)
	}
	// audit 2026-10-12 (GitHub #350): the linked knowledge-graph entity is now
	// withheld from every graph read, and its row carries the residual note.
	withheld := map[string]TableCount{}
	for _, tc := range rep.Withheld {
		withheld[tc.Table] = tc
	}
	if len(rep.Withheld) != 2 || withheld[string(TableProjectMemoryChunks)].Rows != 3 || withheld[string(TableKnowledgeEntities)].Rows != 1 {
		t.Fatalf("withheld = %+v, want 3 memory chunks and 1 knowledge entity", rep.Withheld)
	}
	if note := withheld[string(TableKnowledgeEntities)].Reason; !strings.Contains(note, "resolver") {
		t.Errorf("the withheld entity row must state its residual (the extraction resolver), got %q", note)
	}
	if rep.SharedWithheld != 2 {
		t.Errorf("SharedWithheld = %d, want 2 (unknown exclusivity counts as shared)", rep.SharedWithheld)
	}
	got := map[string]TableCount{}
	for _, tc := range rep.NotWithheld {
		got[tc.Table] = tc
	}
	if _, ok := got[string(TableKnowledgeEntities)]; ok {
		t.Errorf("knowledge entities are withheld now, not listed as not withheld: %+v", rep.NotWithheld)
	}
	for _, table := range []LinkableTable{TableChatAuditLog} {
		tc, ok := got[string(table)]
		if !ok || tc.Rows != 1 {
			t.Errorf("%s missing from NotWithheld: %+v", table, rep.NotWithheld)
			continue
		}
		if len(tc.Reason) < 20 {
			t.Errorf("%s: a not-withheld table needs a stated reason, got %q", table, tc.Reason)
		}
	}
	if rep.Article != "Art 18(1)(d)" {
		t.Errorf("article %q", rep.Article)
	}
	if len(rep.Limitations) == 0 {
		t.Error("the report must state its limitations")
	}
}

// The denominator is exhaustive (change review round 1, F7): with one link in
// EVERY linkable table, each table appears exactly once across the two buckets
// and the buckets sum to the examined count.
func TestRestrictionReport_DenominatorCoversEveryLinkableTable(t *testing.T) {
	r, err := NewRestriction(verifiedRestrictionRequest(), RestrictLegalClaims, "operator", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var links []Link
	for _, name := range LinkableTables() {
		links = append(links, Link{Table: LinkableTable(name), RowID: "r-" + name, Exclusivity: ExclusiveRow})
	}
	rep := BuildRestrictionReport(r, links)
	seen := map[string]int{}
	sum := 0
	for _, tc := range append(append([]TableCount{}, rep.Withheld...), rep.NotWithheld...) {
		seen[tc.Table]++
		sum += tc.Rows
	}
	if sum != rep.RowsExamined || rep.RowsExamined != len(links) {
		t.Fatalf("buckets sum to %d, examined %d, links %d", sum, rep.RowsExamined, len(links))
	}
	for _, name := range LinkableTables() {
		if seen[name] != 1 {
			t.Errorf("%s appears %d times in the report", name, seen[name])
		}
	}
}

// Every linkable table must have a stated enforcement position, so widening the
// closed set forces a decision about restriction rather than a silent gap.
func TestEveryLinkableTableHasARestrictionPosition(t *testing.T) {
	for _, name := range LinkableTables() {
		table := LinkableTable(name)
		if RestrictionWithheld(table) {
			continue
		}
		if reason := restrictionNotWithheldReason(table); len(reason) < 20 {
			t.Errorf("%s: neither withheld nor given a reason (%q)", table, reason)
		}
	}
	if !RestrictionWithheld(TableProjectMemoryChunks) {
		t.Error("memory chunks are the enforced surface of this increment")
	}
	if !RestrictionWithheld(TableKnowledgeEntities) {
		t.Error("knowledge-graph entities are withheld from every graph read (GitHub #350)")
	}
}
