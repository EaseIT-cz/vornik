package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/persistence"
)

// GitHubAppBindingRepository is the Postgres driver of
// persistence.GitHubAppBindingRepository (GitHub #800).
type GitHubAppBindingRepository struct{ db persistence.DBTX }

// NewGitHubAppBindingRepository wires the repository.
func NewGitHubAppBindingRepository(db persistence.DBTX) *GitHubAppBindingRepository {
	return &GitHubAppBindingRepository{db: db}
}

type ghTx struct{ x persistence.DBTX }

func (s ghTx) InsertDelivery(ctx context.Context, id string, now time.Time) (bool, error) {
	res, err := s.x.ExecContext(ctx, `INSERT INTO github_app_deliveries (delivery_id, received_at) VALUES ($1, $2) ON CONFLICT (delivery_id) DO NOTHING`, id, now)
	if err != nil {
		return false, mapDBError(err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s ghTx) PruneDeliveries(ctx context.Context, before time.Time) error {
	_, err := s.x.ExecContext(ctx, `DELETE FROM github_app_deliveries WHERE received_at < $1`, before)
	return mapDBError(err)
}

const ghRepoCols = `repo, installation_id, project_id, state, created_at, updated_at`

func scanGHRepo(sc interface{ Scan(...any) error }) (persistence.GitHubAppRepo, error) {
	var r persistence.GitHubAppRepo
	err := sc.Scan(&r.Repo, &r.InstallationID, &r.ProjectID, &r.State, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func (s ghTx) GetRepo(ctx context.Context, repo string) (*persistence.GitHubAppRepo, error) {
	// FOR UPDATE: two deliveries naming one repository serialise here.
	r, err := scanGHRepo(s.x.QueryRowContext(ctx, `SELECT `+ghRepoCols+` FROM github_app_repositories WHERE repo = $1 FOR UPDATE`, repo))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return &r, nil
}

func ghRepoRows(rows *sql.Rows, err error) ([]persistence.GitHubAppRepo, error) {
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.GitHubAppRepo
	for rows.Next() {
		r, err := scanGHRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s ghTx) ReposOfInstallation(ctx context.Context, id int64) ([]persistence.GitHubAppRepo, error) {
	return ghRepoRows(s.x.QueryContext(ctx, `SELECT `+ghRepoCols+` FROM github_app_repositories WHERE installation_id = $1 ORDER BY repo FOR UPDATE`, id))
}

func (s ghTx) UpsertRepo(ctx context.Context, r persistence.GitHubAppRepo) error {
	_, err := s.x.ExecContext(ctx, `INSERT INTO github_app_repositories (`+ghRepoCols+`) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (repo) DO UPDATE SET installation_id = EXCLUDED.installation_id, project_id = EXCLUDED.project_id,
		state = EXCLUDED.state, updated_at = EXCLUDED.updated_at`,
		r.Repo, r.InstallationID, r.ProjectID, r.State, r.CreatedAt, r.UpdatedAt)
	return mapDBError(err)
}

func (s ghTx) InsertRepo(ctx context.Context, r persistence.GitHubAppRepo) (bool, error) {
	res, err := s.x.ExecContext(ctx, `INSERT INTO github_app_repositories (`+ghRepoCols+`) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (repo) DO NOTHING`, r.Repo, r.InstallationID, r.ProjectID, r.State, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return false, mapDBError(err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s ghTx) SetSuspended(ctx context.Context, id int64, suspended bool, now time.Time) error {
	_, err := s.x.ExecContext(ctx, `INSERT INTO github_app_installations (installation_id, suspended, updated_at) VALUES ($1, $2, $3)
		ON CONFLICT (installation_id) DO UPDATE SET suspended = EXCLUDED.suspended, updated_at = EXCLUDED.updated_at`, id, suspended, now)
	return mapDBError(err)
}

// ApplyInstallationEvent implements the interface.
func (r *GitHubAppBindingRepository) ApplyInstallationEvent(ctx context.Context, ev persistence.GitHubAppInstallationEvent, now time.Time) (persistence.GitHubAppEventResult, error) {
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return persistence.GitHubAppEventResult{}, mapDBError(err)
	}
	exec := r.db
	if ok {
		exec = tx
		defer func() { _ = tx.Rollback() }()
	}
	res, err := persistence.ApplyGitHubAppEvent(ctx, ghTx{x: exec}, ev, now)
	if err != nil {
		return res, err
	}
	if ok {
		return res, mapDBError(tx.Commit())
	}
	return res, nil
}

// ListGitHubAppRepos implements the interface.
func (r *GitHubAppBindingRepository) ListGitHubAppRepos(ctx context.Context) ([]persistence.GitHubAppRepo, error) {
	return ghRepoRows(r.db.QueryContext(ctx, `SELECT `+ghRepoCols+` FROM github_app_repositories ORDER BY repo`))
}

// SuspendedGitHubAppInstallations implements the interface.
func (r *GitHubAppBindingRepository) SuspendedGitHubAppInstallations(ctx context.Context) (map[int64]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT installation_id FROM github_app_installations WHERE suspended`)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetGitHubAppRepoState implements the interface.
func (r *GitHubAppBindingRepository) SetGitHubAppRepoState(ctx context.Context, repo string, from []string, to string, now time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE github_app_repositories SET state = $1, updated_at = $2 WHERE repo = $3 AND state = ANY($4)`,
		to, now, persistence.NormaliseGitHubRepo(repo), pq.Array(from))
	if err != nil {
		return false, mapDBError(err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
