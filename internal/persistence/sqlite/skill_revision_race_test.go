package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/persistence"
)

type skillReproposalWinner struct {
	DBTX
	won bool
}

func (db *skillReproposalWinner) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if !db.won && strings.Contains(query, "description = ?, body = ?") {
		db.won = true
		if _, err := db.DBTX.ExecContext(ctx, `UPDATE project_skills SET body = 'concurrent winning body', version = version + 1 WHERE id = 'race-skill'`); err != nil {
			return nil, err
		}
	}
	return db.DBTX.ExecContext(ctx, query, args...)
}

func TestSkillReproposalDoesNotReuseConcurrentRevision(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Path = ":memory:"
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewSkillRepository(db.DB)
	s := &persistence.Skill{ID: "race-skill", ProjectID: "p1", Name: "race", Body: "first", Description: "test", BodySHA256: "hash"}
	if err := repo.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	raceRepo := NewSkillRepository(&skillReproposalWinner{DBTX: db.DB})
	s.Body = "losing proposal"
	if _, err := raceRepo.Upsert(ctx, s); !errors.Is(err, persistence.ErrSkillRevisionConflict) {
		t.Fatalf("lost race: %v", err)
	}
	got, err := repo.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.Body != "concurrent winning body" {
		t.Fatalf("winner overwritten: %+v", got)
	}
}

type skillFailedExec struct {
	DBTX
	rowsError bool
}
type skillFailedResult struct{}

func (skillFailedResult) LastInsertId() (int64, error) { return 0, errors.New("result unavailable") }
func (skillFailedResult) RowsAffected() (int64, error) { return 0, errors.New("result unavailable") }
func (db skillFailedExec) ExecContext(context.Context, string, ...interface{}) (sql.Result, error) {
	if db.rowsError {
		return skillFailedResult{}, nil
	}
	return nil, errors.New("database unavailable")
}

func TestSkillRevisionReviewDatabaseErrors(t *testing.T) {
	ctx := context.Background()
	for _, rowsError := range []bool{false, true} {
		repo := NewSkillRepository(skillFailedExec{rowsError: rowsError})
		if err := repo.SetMaturityForVersion(ctx, "id", 1, "active"); err == nil {
			t.Fatal("database error ignored")
		}
	}
}

func TestSkillReproposalConflictAndDatabaseErrors(t *testing.T) {
	for _, mode := range []string{"get", "insert", "archive", "update", "result", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			initial := mock.ExpectQuery("FROM project_skills")
			failure := errors.New("database unavailable")
			switch mode {
			case "get":
				initial.WillReturnError(failure)
			case "insert":
				initial.WillReturnRows(sqlmock.NewRows([]string{"id"}))
				mock.ExpectExec("INSERT INTO project_skills").WillReturnError(failure)
			default:
				now := sqliteTime(time.Now().UTC())
				columns := strings.FieldsFunc(skillColumns, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
				initial.WillReturnRows(sqlmock.NewRows(columns).AddRow("id", "p1", nil, "name", "d", "body", "hash", nil, "[]", "[]", "draft", 1, nil, nil, nil, 0, 0, 0, nil, now, now, false, "", "", "", "", now, false))
				archive := mock.ExpectExec("INSERT OR IGNORE INTO project_skill_versions")
				if mode == "archive" {
					archive.WillReturnError(failure)
				} else {
					archive.WillReturnResult(sqlmock.NewResult(0, 1))
					update := mock.ExpectExec("UPDATE project_skills SET")
					switch mode {
					case "update":
						update.WillReturnError(failure)
					case "result":
						update.WillReturnResult(sqlmock.NewErrorResult(failure))
					case "conflict":
						update.WillReturnResult(sqlmock.NewResult(0, 0))
					}
				}
			}
			_, err = NewSkillRepository(db).Upsert(context.Background(), &persistence.Skill{ProjectID: "p1", Name: "name"})
			if err == nil {
				t.Fatal("re-proposal error ignored")
			}
			if mode == "conflict" && !errors.Is(err, persistence.ErrSkillRevisionConflict) {
				t.Fatalf("stale update: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
