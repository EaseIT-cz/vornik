//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memory/graph"
)

// GDPR increment 6 (GitHub #343) against a real database: the restriction record,
// the live restriction predicate on every SQL use path, entity rectification, and
// the content_title change to the shared 5c transaction.
//
// audit 2026-10-12: Art 16 and Art 18 requests were recorded and nothing executed
// them (DPA §8 limitation 7).
//
// Design: https://docs.vornik.io §4.16

const inc6Project = "gdpr-inc6-test"

func resetInc6(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM data_subject_restrictions WHERE subject_id LIKE 'ds_inc6_%'`,
		`DELETE FROM data_subject_requests WHERE subject_id LIKE 'ds_inc6_%'`,
		`DELETE FROM data_subject_links WHERE subject_id LIKE 'ds_inc6_%'`,
		`DELETE FROM data_subjects WHERE id LIKE 'ds_inc6_%'`,
		`DELETE FROM knowledge_entities WHERE project_id = '` + inc6Project + `'`,
		`DELETE FROM project_memory_chunks WHERE project_id = '` + inc6Project + `'`,
	} {
		if _, err := db.DB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("reset: %s: %v", stmt, err)
		}
	}
}

// seedRestrictedFixture: subject ds_inc6_a linked to chunk ck_a; ck_b unlinked;
// a verified restriction request.
func seedRestrictedFixture(t *testing.T, db *DB) (*DataSubjectRepository, datasubject.Request) {
	t.Helper()
	resetInc6(t, db)
	t.Cleanup(func() { resetInc6(t, db) })
	ctx := context.Background()
	repo := NewDataSubjectRepository(db.DB)
	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: "ds_inc6_a", DisplayName: "Jane Doe"}); err != nil {
		t.Fatalf("subject: %v", err)
	}
	seedChunkInProject(t, db, "ck_a", inc6Project, "Jane Doe works at Acme")
	seedChunkInProject(t, db, "ck_b", inc6Project, "An unrelated note")
	if err := repo.AddLink(ctx, "ds_inc6_a", datasubject.Link{
		Table: datasubject.TableProjectMemoryChunks, RowID: "ck_a", ProjectID: inc6Project,
		Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceProbable,
		Exclusivity: datasubject.ExclusiveRow,
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	req := datasubject.Request{ID: "dsr_inc6_1", SubjectID: "ds_inc6_a", Kind: datasubject.RequestRestriction,
		State: datasubject.StateOpen, OpenedAt: time.Now().UTC()}
	if err := repo.CreateRequest(ctx, req); err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := req.Verify("operator", "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	return repo, req
}

func TestIntegrationRestriction_EveryUsePathHonoursItUntilLifted(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo, req := seedRestrictedFixture(t, db)
	mem := memory.NewRepository(db.DB)
	lookup := graph.NewSQLChunkLookup(db.DB)

	visible := func() (screen, lookupSees, contents bool) {
		t.Helper()
		r, err := mem.RestrictedChunks(ctx, []string{"ck_a", "ck_b"})
		if err != nil {
			t.Fatalf("RestrictedChunks: %v", err)
		}
		if r["ck_b"] {
			t.Fatal("an unlinked chunk was reported restricted")
		}
		got, err := lookup.LookupChunks(ctx, inc6Project, []string{"ck_a"}, "")
		if err != nil {
			t.Fatalf("LookupChunks: %v", err)
		}
		texts, err := mem.ListChunkContents(ctx, inc6Project, 50)
		if err != nil {
			t.Fatalf("ListChunkContents: %v", err)
		}
		inContents := false
		for _, s := range texts {
			if s == "Jane Doe works at Acme" {
				inContents = true
			}
		}
		return !r["ck_a"], len(got) == 1, inContents
	}

	if s, l, c := visible(); !s || !l || !c {
		t.Fatalf("before any restriction the chunk must be usable: screen=%v lookup=%v contents=%v", s, l, c)
	}

	res, err := datasubject.NewRestriction(req, datasubject.RestrictAccuracyContested, "operator", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateRestriction(ctx, res); err != nil {
		t.Fatalf("CreateRestriction: %v", err)
	}
	if s, l, c := visible(); s || l || c {
		t.Fatalf("a restricted chunk was usable: screen=%v lookup=%v contents=%v", s, l, c)
	}

	// One active restriction per subject — the partial unique index.
	second := res
	second.ID = res.ID + "_2"
	if err := repo.CreateRestriction(ctx, second); !errors.Is(err, datasubject.ErrRestrictionActive) {
		t.Fatalf("a second active restriction must be refused, got %v", err)
	}

	active, err := repo.ActiveRestriction(ctx, "ds_inc6_a")
	if err != nil || active.ID != res.ID || active.Ground != datasubject.RestrictAccuracyContested {
		t.Fatalf("ActiveRestriction = %+v, %v", active, err)
	}
	if err := active.Lift("operator", "accuracy verified", true, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveRestrictionLift(ctx, active); err != nil {
		t.Fatalf("SaveRestrictionLift: %v", err)
	}
	if s, l, c := visible(); !s || !l || !c {
		t.Fatalf("a lift must take effect at once: screen=%v lookup=%v contents=%v", s, l, c)
	}
	// The lifted row is kept, not deleted (Art 5(2) evidence), and cannot be lifted twice.
	if err := repo.SaveRestrictionLift(ctx, active); err == nil {
		t.Fatal("lifting an already-lifted restriction must be refused by the store")
	}
	var n int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM data_subject_restrictions WHERE subject_id = 'ds_inc6_a' AND lifted_at IS NOT NULL`,
	).Scan(&n); err != nil || n != 1 {
		t.Fatalf("lifted row count %d (%v), want 1 retained", n, err)
	}
	if _, err := repo.ActiveRestriction(ctx, "ds_inc6_a"); !errors.Is(err, datasubject.ErrNoActiveRestriction) {
		t.Fatalf("no active restriction expected, got %v", err)
	}
}

// The schema refuses a lift row without the Art 18(3) affirmation, so a writer
// that bypassed the domain type still cannot record one.
func TestIntegrationRestriction_SchemaRefusesALiftWithoutTheAffirmation(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo, req := seedRestrictedFixture(t, db)
	res, err := datasubject.NewRestriction(req, datasubject.RestrictLegalClaims, "operator", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateRestriction(ctx, res); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE data_subject_restrictions SET lifted_at = NOW(), lifted_by = 'x', lift_reason = 'y' WHERE id = $1`,
		res.ID); err == nil {
		t.Fatal("a lift with no subject_informed_at must violate the CHECK")
	}
}

func seedEntityFull(t *testing.T, db *DB, id, name, aliasesJSON, description string) {
	t.Helper()
	if _, err := db.DB.Exec(`
		INSERT INTO knowledge_entities (id, project_id, type, canonical_name, aliases, description, embedding)
		VALUES ($1, $2, 'PERSON', $3, $4::jsonb, $5, $6)`,
		id, inc6Project, name, aliasesJSON, description, pgVectorLiteral(1024)); err != nil {
		t.Fatalf("seed entity %s: %v", id, err)
	}
}

func TestIntegrationEntityRectifier(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	resetInc6(t, db)
	t.Cleanup(func() { resetInc6(t, db) })
	r := NewEntityRectifierRepository(db.DB)

	seedEntityFull(t, db, "ent_inc6_1", "Jane Do", `["J. Do","Janey"]`, "Jane Do of Acme")
	seedEntityFull(t, db, "ent_inc6_2", "Jane Doe", `[]`, "")

	before, err := r.LoadEntity(ctx, "ent_inc6_1")
	if err != nil || before.CanonicalName != "Jane Do" || len(before.Aliases) != 2 {
		t.Fatalf("LoadEntity = %+v, %v", before, err)
	}

	// Colliding with another entity's (project, type, canonical_name) is a
	// conflict, never a merge, and writes nothing.
	clash := datasubject.EntityText{CanonicalName: "Jane Doe", Aliases: before.Aliases, Description: before.Description}
	out, err := r.RectifyEntity(ctx, "ent_inc6_1", before, clash)
	if err != nil || out != datasubject.EntityRectifyConflict {
		t.Fatalf("collision: %q %v", out, err)
	}

	// Stale expectation: version guard, nothing written.
	stale := before
	stale.Description = "something else"
	upd := datasubject.EntityText{CanonicalName: "Jane Dough", Aliases: []string{"J. Dough", "Janey"}, Description: "Jane Dough of Acme"}
	if out, err := r.RectifyEntity(ctx, "ent_inc6_1", stale, upd); err != nil || out != datasubject.EntityRectifyVersionChanged {
		t.Fatalf("stale: %q %v", out, err)
	}

	out, err = r.RectifyEntity(ctx, "ent_inc6_1", before, upd)
	if err != nil || out != datasubject.EntityRectifyApplied {
		t.Fatalf("apply: %q %v", out, err)
	}
	after, err := r.LoadEntity(ctx, "ent_inc6_1")
	if err != nil || after.CanonicalName != "Jane Dough" || after.Aliases[0] != "J. Dough" || after.Description != "Jane Dough of Acme" {
		t.Fatalf("after = %+v, %v", after, err)
	}
	var hasEmbed bool
	if err := db.DB.QueryRowContext(ctx, `SELECT embedding IS NOT NULL FROM knowledge_entities WHERE id = 'ent_inc6_1'`).Scan(&hasEmbed); err != nil {
		t.Fatal(err)
	}
	if hasEmbed {
		t.Fatal("the embedding derived from the inaccurate name must be cleared")
	}
}

// 5c amendment (2026-10-12): the shared redaction/rectification transaction clears
// the LLM-written title derived from the old content, in the same UPDATE.
func TestIntegrationRedactChunk_ClearsTheDerivedTitle(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := NewChunkRedactorRepository(db.DB)
	c := seedChunk(t, db, "redact-title-1", "Called jane@example.com; Peter joined.")
	cleanRedactTest(t, db)
	if _, err := db.ExecContext(ctx,
		`UPDATE project_memory_chunks SET content_title = 'Call with Jane Doe' WHERE id = $1`, c.id); err != nil {
		t.Fatal(err)
	}
	out, err := repo.RedactChunk(ctx, c.id, c.hash, "Called the client; Peter joined.")
	if err != nil || out.Outcome != datasubject.RedactionApplied {
		t.Fatalf("redact: %+v %v", out, err)
	}
	var title *string
	if err := db.QueryRowContext(ctx, `SELECT content_title FROM project_memory_chunks WHERE id = $1`, c.id).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != nil {
		t.Fatalf("the pre-change title survived the rewrite: %q", *title)
	}
}
