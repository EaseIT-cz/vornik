package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"vornik.io/vornik/internal/datasubject"
)

// GDPR increment 6 (GitHub #343) store outcomes, checked without a database. The
// semantics (partial index, CHECK, JSONB guard) are pinned by
// data_subject_increment6_integration_test.go in the pgvector lane.
//
// audit 2026-10-12: Art 16/18 requests were recorded and nothing executed them.

func activeRestriction() datasubject.Restriction {
	return datasubject.Restriction{ID: "dsx_1", SubjectID: "ds_1", RequestID: "dsr_1",
		Ground: datasubject.RestrictAccuracyContested, RestrictedBy: "op", RestrictedAt: time.Now()}
}

func TestCreateRestriction_SecondActiveIsErrRestrictionActive(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// The parent subject and its request are read in the same transaction
	// first (GitHub #355: SQLite enforces no foreign key, so the shared
	// repository states the rule itself).
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM data_subjects")).
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM data_subject_requests")).
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO data_subject_restrictions")).
		WillReturnError(&pq.Error{Code: "23505"})
	mock.ExpectRollback()
	err = NewDataSubjectRepository(db).CreateRestriction(context.Background(), activeRestriction())
	if !errors.Is(err, datasubject.ErrRestrictionActive) {
		t.Fatalf("got %v, want ErrRestrictionActive", err)
	}
	lifted := activeRestriction()
	lifted.LiftedAt = time.Now()
	if err := NewDataSubjectRepository(db).CreateRestriction(context.Background(), lifted); err == nil {
		t.Fatal("recording an already-lifted restriction as new must be refused")
	}
}

func TestActiveRestriction_NoneIsErrNoActiveRestriction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(regexp.QuoteMeta("FROM data_subject_restrictions")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if _, err := NewDataSubjectRepository(db).ActiveRestriction(context.Background(), "ds_1"); !errors.Is(err, datasubject.ErrNoActiveRestriction) {
		t.Fatalf("got %v", err)
	}
}

func TestSaveRestrictionLift_GuardsOnStillActive(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := NewDataSubjectRepository(db)
	r := activeRestriction()
	if err := repo.SaveRestrictionLift(context.Background(), r); err == nil {
		t.Fatal("saving an un-lifted restriction as a lift must be refused")
	}
	if err := r.Lift("op", "verified", true, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta("WHERE id = $1 AND lifted_at IS NULL")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.SaveRestrictionLift(context.Background(), r); err == nil {
		t.Fatal("a lift that matched no active row must be an error, not silent success")
	}
}

func TestRectifyEntity_Outcomes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	r := NewEntityRectifierRepository(db)
	exp := datasubject.EntityText{CanonicalName: "Jane Do"}
	upd := datasubject.EntityText{CanonicalName: "Jane Doe"}
	stmt := regexp.QuoteMeta("embedding = NULL")

	mock.ExpectExec(stmt).WillReturnError(&pq.Error{Code: "23505"})
	if out, err := r.RectifyEntity(context.Background(), "e1", exp, upd); err != nil || out != datasubject.EntityRectifyConflict {
		t.Fatalf("duplicate: %q %v", out, err)
	}
	mock.ExpectExec(stmt).WillReturnResult(sqlmock.NewResult(0, 0))
	if out, err := r.RectifyEntity(context.Background(), "e1", exp, upd); err != nil || out != datasubject.EntityRectifyVersionChanged {
		t.Fatalf("stale: %q %v", out, err)
	}
	mock.ExpectExec(stmt).WithArgs("e1", "Jane Do", "[]", "", "Jane Doe", "[]", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if out, err := r.RectifyEntity(context.Background(), "e1", exp, upd); err != nil || out != datasubject.EntityRectifyApplied {
		t.Fatalf("apply: %q %v", out, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
