package datasubject

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// GDPR increment 6 (GitHub #343), Art 16 rectification.
//
// audit 2026-10-12: Art 16 requests were recorded in the ledger and nothing could
// execute them (DPA §8 limitation 7). Rectification reuses the slice-5c chunk
// rewrite transaction (ChunkRedactor) and never deletes.
//
// see LLD § https://docs.vornik.io §4.16.2

// zeroWidthSpace is U+200B, built from its code point so the source stays visible.
const zeroWidthSpace = string(rune(0x200B))

func verifiedRectificationRequest() Request {
	return Request{
		ID: "dsr_r", SubjectID: "ds_1", Kind: RequestRectification,
		State: StateVerified, OpenedAt: time.Now().Add(-time.Hour),
		VerifiedBy: "operator", VerifiedHow: "email round-trip",
	}
}

func TestCorrection_Validate(t *testing.T) {
	for _, c := range []Correction{
		{Incorrect: "", Correct: "x"},
		{Incorrect: "x", Correct: " "},
		{Incorrect: "Jane Doe", Correct: "jane doe"}, // case-only: not verifiable
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v must be refused", c)
		}
	}
	if err := (Correction{Incorrect: "Acme", Correct: "Beta"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCorrection_ApplyIsCaseInsensitiveAndLiteral(t *testing.T) {
	c := Correction{Incorrect: "works at Acme (UK)", Correct: "works at Beta"}
	out, n := c.Apply("Jane WORKS AT ACME (UK). Bob works at Acme (UK) too.")
	if n != 2 {
		t.Fatalf("replaced %d, want 2", n)
	}
	if out != "Jane works at Beta. Bob works at Beta too." {
		t.Fatalf("got %q", out)
	}
	// Regex metacharacters in the operator's text are literal.
	if _, n := (Correction{Incorrect: "a.c", Correct: "x"}).Apply("abc"); n != 0 {
		t.Error("'.' must not act as a wildcard")
	}
}

// "Jane Do" → "Jane Doe": the incorrect text is a substring of the correct one,
// so a naive "incorrect must be absent" check would reject every such correction.
func TestVerifyRectification(t *testing.T) {
	c := Correction{Incorrect: "Jane Do", Correct: "Jane Doe"}
	if err := VerifyRectification(c, "Met Jane Doe today."); err != nil {
		t.Fatalf("a correct output failed verification: %v", err)
	}
	if err := VerifyRectification(c, "Met Jane Doe and Jane Do today."); err == nil {
		t.Error("a surviving inaccuracy must fail verification")
	}
	if err := VerifyRectification(c, "Met someone today."); err == nil {
		t.Error("an output that lost the correction must fail verification")
	}
	// Normalisation: a zero-width split must not hide a survivor.
	c2 := Correction{Incorrect: "born 1980", Correct: "born 1981"}
	if err := VerifyRectification(c2, "born 1981; also born 19"+zeroWidthSpace+"80"); err == nil {
		t.Error("a zero-width-split survivor must fail verification")
	}
}

func TestPlanRectification_ClassifiesEveryLinkedRow(t *testing.T) {
	c := Correction{Incorrect: "Acme", Correct: "Beta"}
	items := []Item{
		{Table: TableProjectMemoryChunks, RowID: "c1", Content: "Jane works at Acme"},
		{Table: TableProjectMemoryChunks, RowID: "c2", Content: "unrelated"},
		{Table: TableKnowledgeEntities, RowID: "e1", Content: "Jane Doe of ACME"},
		{Table: TableChatAuditLog, RowID: "a1", Content: "I work at Acme"},
		{Table: TableOperatorProfile, RowID: "o1", Content: "employer: Acme"},
	}
	plan, err := PlanRectification(verifiedRectificationRequest(), items, c)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Examined != 5 || plan.Unmatched != 1 {
		t.Fatalf("examined %d unmatched %d, want 5 and 1 — the denominator", plan.Examined, plan.Unmatched)
	}
	want := map[string]RectifyTreatment{
		"c1": TreatCorrect, "e1": TreatCorrect,
		"a1": TreatSupplementaryStatement, "o1": TreatManual,
	}
	if len(plan.Actions) != len(want) {
		t.Fatalf("actions %+v", plan.Actions)
	}
	for _, a := range plan.Actions {
		if want[a.RowID] != a.Treatment {
			t.Errorf("%s: treatment %q, want %q", a.RowID, a.Treatment, want[a.RowID])
		}
	}

	open := verifiedRectificationRequest()
	open.State = StateOpen
	if _, err := PlanRectification(open, items, c); err == nil {
		t.Error("an unverified request must not reach rectification")
	}
	wrong := verifiedRectificationRequest()
	wrong.Kind = RequestAccess
	if _, err := PlanRectification(wrong, items, c); err == nil {
		t.Error("only a rectification request may rectify")
	}
}

// --- executor ---

type fakeChunkStore struct {
	content map[string]string
	outcome RedactionOutcome
	written map[string]string
	deleted []string
}

func (f *fakeChunkStore) LoadChunk(_ context.Context, id string) (string, string, error) {
	c, ok := f.content[id]
	if !ok {
		return "", "", errors.New("not found")
	}
	return c, "h-" + id, nil
}

func (f *fakeChunkStore) RedactChunk(_ context.Context, id, expected, content string) (RedactionResult, error) {
	if expected != "h-"+id {
		return RedactionResult{}, errors.New("wrong version token")
	}
	switch f.outcome {
	case RedactionCollision:
		return RedactionResult{Outcome: RedactionCollision, SurvivorID: "other"}, nil
	case RedactionVersionChanged:
		return RedactionResult{Outcome: RedactionVersionChanged}, nil
	}
	if f.written == nil {
		f.written = map[string]string{}
	}
	f.written[id] = content
	return RedactionResult{Outcome: RedactionApplied, NewHash: "n-" + id}, nil
}

type fakeEntityStore struct {
	text    map[string]EntityText
	outcome EntityRectifyOutcome
	written map[string]EntityText
}

func (f *fakeEntityStore) LoadEntity(_ context.Context, id string) (EntityText, error) {
	t, ok := f.text[id]
	if !ok {
		return EntityText{}, errors.New("not found")
	}
	return t, nil
}

func (f *fakeEntityStore) RectifyEntity(_ context.Context, id string, _, updated EntityText) (EntityRectifyOutcome, error) {
	if f.outcome != "" {
		return f.outcome, nil
	}
	if f.written == nil {
		f.written = map[string]EntityText{}
	}
	f.written[id] = updated
	return EntityRectifyApplied, nil
}

func approveAllRect(RectificationProposal) (bool, error) { return true, nil }

func rectifyPlan(t *testing.T, items []Item) *RectificationPlan {
	t.Helper()
	plan, err := PlanRectification(verifiedRectificationRequest(), items, Correction{Incorrect: "Acme", Correct: "Beta"})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestRectifier_CorrectsChunksAndEntitiesAndRetainsTheOriginal(t *testing.T) {
	chunks := &fakeChunkStore{content: map[string]string{"c1": "Jane works at Acme"}}
	ents := &fakeEntityStore{text: map[string]EntityText{
		"e1": {CanonicalName: "Acme employee Jane", Aliases: []string{"Jane of Acme", "J."}, Description: "works at acme"},
	}}
	var proposals []RectificationProposal
	r := &Rectifier{Chunks: chunks, Entities: ents, Approve: func(p RectificationProposal) (bool, error) {
		proposals = append(proposals, p)
		return true, nil
	}}
	plan := rectifyPlan(t, []Item{
		{Table: TableProjectMemoryChunks, RowID: "c1", Content: "Jane works at Acme"},
		{Table: TableKnowledgeEntities, RowID: "e1", Content: "Acme employee Jane"},
		{Table: TableTaskMessages, RowID: "m1", Content: "she said Acme"},
	})
	res, err := r.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete() {
		t.Fatalf("expected complete, got deferred=%+v failed=%+v manual=%+v", res.Deferred, res.Failed, res.Manual)
	}
	if chunks.written["c1"] != "Jane works at Beta" {
		t.Errorf("chunk written %q", chunks.written["c1"])
	}
	got := ents.written["e1"]
	if got.CanonicalName != "Beta employee Jane" || got.Aliases[0] != "Jane of Beta" || got.Aliases[1] != "J." ||
		got.Description != "works at Beta" {
		t.Errorf("entity written %+v", got)
	}
	if len(proposals) != 2 {
		t.Errorf("every change must be reviewed: %d proposals", len(proposals))
	}
	if len(res.Corrected) != 2 {
		t.Fatalf("corrected %+v", res.Corrected)
	}
	for _, cr := range res.Corrected {
		if cr.Before == "" || cr.After == "" || cr.Before == cr.After {
			t.Errorf("%s: the original must be retained alongside the correction: %+v", cr.RowID, cr)
		}
	}
	if len(res.Statements) != 1 || res.Statements[0].RowID != "m1" ||
		!strings.Contains(res.Statements[0].Statement, "Beta") {
		t.Errorf("transcript must get a supplementary statement, not an edit: %+v", res.Statements)
	}
	narr := res.Narrative()
	if !strings.Contains(narr, "original retained in the audit record") {
		t.Errorf("the subject must be told the original is retained (review Q6): %q", narr)
	}
}

// Rectification never deletes: where 5c deletes a chunk whose rewrite collides,
// a correction defers it for a human.
func TestRectifier_CollisionAndVersionChangeDeferNeverDelete(t *testing.T) {
	for _, oc := range []RedactionOutcome{RedactionCollision, RedactionVersionChanged} {
		chunks := &fakeChunkStore{content: map[string]string{"c1": "at Acme"}, outcome: oc}
		r := &Rectifier{Chunks: chunks, Entities: &fakeEntityStore{}, Approve: approveAllRect}
		res, err := r.Execute(context.Background(), rectifyPlan(t, []Item{
			{Table: TableProjectMemoryChunks, RowID: "c1", Content: "at Acme"},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Complete() || len(res.Deferred) != 1 || len(res.Corrected) != 0 {
			t.Errorf("%s: want one deferral, got %+v", oc, res)
		}
		if len(chunks.deleted) != 0 {
			t.Errorf("%s: rectification deleted a row", oc)
		}
	}
	ents := &fakeEntityStore{text: map[string]EntityText{"e1": {CanonicalName: "Acme"}}, outcome: EntityRectifyConflict}
	r := &Rectifier{Chunks: &fakeChunkStore{}, Entities: ents, Approve: approveAllRect}
	res, _ := r.Execute(context.Background(), rectifyPlan(t, []Item{
		{Table: TableKnowledgeEntities, RowID: "e1", Content: "Acme"},
	}))
	if res.Complete() || len(res.Deferred) != 1 {
		t.Errorf("an entity name collision must defer: %+v", res)
	}
}

func TestRectifier_ReviewIsMandatory(t *testing.T) {
	chunks := &fakeChunkStore{content: map[string]string{"c1": "at Acme"}}
	items := []Item{{Table: TableProjectMemoryChunks, RowID: "c1", Content: "at Acme"}}

	// No reviewer wired: defer, never apply.
	r := &Rectifier{Chunks: chunks, Entities: &fakeEntityStore{}}
	res, _ := r.Execute(context.Background(), rectifyPlan(t, items))
	if len(res.Deferred) != 1 || len(chunks.written) != 0 {
		t.Fatalf("an unreviewed correction must not be written: %+v", res)
	}
	// Declined.
	r.Approve = func(RectificationProposal) (bool, error) { return false, nil }
	res, _ = r.Execute(context.Background(), rectifyPlan(t, items))
	if len(res.Deferred) != 1 || len(chunks.written) != 0 {
		t.Fatalf("a declined correction must not be written: %+v", res)
	}
}

// The reason review has no bypass is the cross-row false match ("Acme" in another
// record that means something else). Each row is reviewed on its own, and a
// declined one defers WITHOUT blocking an approved one (change review round 1, F8).
func TestRectifier_MixedPerRowReviewOutcomes(t *testing.T) {
	chunks := &fakeChunkStore{content: map[string]string{
		"c1": "Jane works at Acme", "c2": "Acme Corp, unrelated vendor",
	}}
	r := &Rectifier{Chunks: chunks, Entities: &fakeEntityStore{}, Approve: func(p RectificationProposal) (bool, error) {
		return p.RowID == "c1", nil
	}}
	res, err := r.Execute(context.Background(), rectifyPlan(t, []Item{
		{Table: TableProjectMemoryChunks, RowID: "c1", Content: "Jane works at Acme"},
		{Table: TableProjectMemoryChunks, RowID: "c2", Content: "Acme Corp, unrelated vendor"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Corrected) != 1 || res.Corrected[0].RowID != "c1" {
		t.Fatalf("the approved row must be corrected: %+v", res.Corrected)
	}
	if len(res.Deferred) != 1 || res.Deferred[0].RowID != "c2" {
		t.Fatalf("the declined row must defer: %+v", res.Deferred)
	}
	if _, wrote := chunks.written["c2"]; wrote {
		t.Fatal("a declined row was written")
	}
	if res.Complete() {
		t.Fatal("a request with a deferred row must not be complete")
	}
}

// The match is re-checked at execution: a row whose text no longer holds the
// inaccuracy (or held it only in its generated title) is deferred with the reason,
// never "corrected" with an unchanged write.
func TestRectifier_RowNoLongerMatchingIsDeferred(t *testing.T) {
	chunks := &fakeChunkStore{content: map[string]string{"c1": "nothing here"}}
	r := &Rectifier{Chunks: chunks, Entities: &fakeEntityStore{}, Approve: approveAllRect}
	res, err := r.Execute(context.Background(), rectifyPlan(t, []Item{
		{Table: TableProjectMemoryChunks, RowID: "c1", Content: "Acme title\nnothing here"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deferred) != 1 || len(chunks.written) != 0 {
		t.Fatalf("want a deferral and no write, got %+v", res)
	}
}

func TestRectifier_RefusesHalfWiredExecutor(t *testing.T) {
	plan := rectifyPlan(t, []Item{{Table: TableProjectMemoryChunks, RowID: "c1", Content: "Acme"}})
	if _, err := (&Rectifier{}).Execute(context.Background(), plan); err == nil {
		t.Error("an executor without stores must refuse")
	}
	if _, err := (&Rectifier{Chunks: &fakeChunkStore{}, Entities: &fakeEntityStore{}}).Execute(context.Background(), nil); err == nil {
		t.Error("a nil plan must be refused")
	}
}
