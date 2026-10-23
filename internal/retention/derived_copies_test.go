package retention

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

// expectDerivedCopiesAbsent queues the probes of the two derived-copy steps
// (GitHub #394) that close every global sweep, both answering "absent", so a
// sqlmock case about another step stays about that step.
func expectDerivedCopiesAbsent(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.blackbox_trace_cache')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.project_gists')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
}

// The derived-copy steps run on their defaults when the policy is zero: zero
// is the default horizon, never "keep forever" (design §8.2.1, §8.2.2).
func TestSweepGlobal_DerivedCopiesZeroMeansTheDefault(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	s := New(db, zerolog.Nop())
	expectGlobalSweepThroughRatings(mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.config_apply_journal')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.blackbox_trace_cache')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM blackbox_trace_cache")).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(10))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blackbox_trace_cache WHERE assembled_at < $1")).
		WithArgs(withinOf{want: now.AddDate(0, 0, -DefaultBlackboxTraceCacheDays), tolerance: time.Minute}).
		WillReturnResult(sqlmock.NewResult(0, 3))

	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.project_gists')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM project_gists")).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(5))
	gistThreshold := withinOf{want: now.AddDate(0, 0, -DefaultProjectGistsDays), tolerance: time.Minute}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_gists")).
		WithArgs(gistThreshold).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM project_gists WHERE generated_at < $1")).
		WithArgs(gistThreshold).
		WillReturnResult(sqlmock.NewResult(0, 2))

	g, err := s.SweepGlobal(context.Background(), GlobalPolicy{})
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if g.BlackboxTraceCache != 3 || g.BlackboxTraceCacheExamined != 10 ||
		g.ProjectGists != 2 || g.ProjectGistNarratives != 1 || g.ProjectGistsExamined != 5 {
		t.Fatalf("counts = %+v", g)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("zero days did not resolve to the default horizons: %v", err)
	}
}
