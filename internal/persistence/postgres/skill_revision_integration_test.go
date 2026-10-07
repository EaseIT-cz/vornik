//go:build integration

package postgres

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func TestSkillProposalDateMigrationLegacyRows(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var migration persistence.Migration
	for _, m := range persistence.DefaultMigrations {
		if m.Version == 216 {
			migration = m
		}
	}
	if migration.Version != 216 {
		t.Fatal("missing proposal-date migration")
	}
	if _, err := tx.ExecContext(ctx, migration.Down); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_skills
 (id,project_id,name,description,body,body_sha256,maturity,version,created_at,updated_at)
 VALUES ('legacy-date-test','p1','legacy-date-test','d','new','sha','draft',2,'2026-01-01','2026-02-01');
 INSERT INTO project_skill_versions (id,skill_id,version,name,description,body,body_sha256,maturity,archived_at)
 VALUES ('old-date-test','legacy-date-test',1,'legacy-date-test','d','old','sha','active','2026-02-01');`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, migration.Up); err != nil {
		t.Fatal(err)
	}
	repo := NewSkillRepository(tx)
	live, err := repo.GetByID(ctx, "legacy-date-test")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := repo.ListVersions(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !live.ProposalDateEstimated || !live.ProposedAt.Equal(live.UpdatedAt) {
		t.Fatalf("live estimate: %+v", live)
	}
	if len(archive) != 1 || !archive[0].ProposalDateEstimated || !archive[0].ProposedAt.Equal(archive[0].ArchivedAt) {
		t.Fatalf("archive estimate: %+v", archive)
	}
	next, err := repo.Upsert(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if next.ProposalDateEstimated || !next.ProposedAt.After(live.ProposedAt) {
		t.Fatalf("exact reproposal: %+v", next)
	}
	if _, err := tx.ExecContext(ctx, migration.Up); err != nil {
		t.Fatal(err)
	}
	reread, err := repo.GetByID(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reread.ProposalDateEstimated || !reread.ProposedAt.Equal(next.ProposedAt) {
		t.Fatal("rerunning migration modified exact date")
	}
}
