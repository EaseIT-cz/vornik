package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/persistence"
)

func TestSkillRevisionReviewDatabaseErrors(t *testing.T) {
	for _, mode := range []string{"exec", "rows"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, cleanup := newMockDBTX(t)
			defer cleanup()
			expected := mock.ExpectExec("UPDATE project_skills SET maturity")
			if mode == "exec" {
				expected.WillReturnError(errors.New("database unavailable"))
			} else {
				expected.WillReturnResult(sqlmock.NewErrorResult(errors.New("result unavailable")))
			}
			if err := NewSkillRepository(db).SetMaturityForVersion(context.Background(), "id", 1, "active"); err == nil {
				t.Fatal("database error ignored")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSkillReproposalConflictAndDatabaseErrors(t *testing.T) {
	for _, mode := range []string{"get", "insert", "archive", "update", "result", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, cleanup := newMockDBTX(t)
			defer cleanup()
			initial := mock.ExpectQuery("FROM project_skills")
			failure := errors.New("database unavailable")
			switch mode {
			case "get":
				initial.WillReturnError(failure)
			case "insert":
				initial.WillReturnRows(sqlmock.NewRows([]string{"id"}))
				mock.ExpectExec("INSERT INTO project_skills").WillReturnError(failure)
			default:
				now := time.Now().UTC()
				columns := strings.FieldsFunc(pgSkillColumns, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
				initial.WillReturnRows(sqlmock.NewRows(columns).AddRow("id", "p1", nil, "name", "d", "body", "hash", nil, "[]", "[]", "draft", 1, nil, nil, nil, 0, 0, 0, nil, now, now, false, "", "", "", "", now, false))
				archive := mock.ExpectExec("INSERT INTO project_skill_versions")
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
			_, err := NewSkillRepository(db).Upsert(context.Background(), &persistence.Skill{ProjectID: "p1", Name: "name"})
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
