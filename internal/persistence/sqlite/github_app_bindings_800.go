package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHubAppBindingRepository is the SQLite driver of
// persistence.GitHubAppBindingRepository (GitHub #800).
type GitHubAppBindingRepository struct{ db *sql.DB }

// NewGitHubAppBindingRepository wires the repository.
func NewGitHubAppBindingRepository(db *sql.DB) *GitHubAppBindingRepository {
	return &GitHubAppBindingRepository{db: db}
}

type ghTx struct{ tx *sql.Tx }

func (s ghTx) InsertDelivery(ctx context.Context, id string, now time.Time) (bool, error) {
	res, err := s.tx.ExecContext(ctx, `INSERT INTO github_app_deliveries (delivery_id, received_at) VALUES (?, ?) ON CONFLICT (delivery_id) DO NOTHING`, id, sqliteTime(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s ghTx) PruneDeliveries(ctx context.Context, before time.Time) error {
	_, err := s.tx.ExecContext(ctx, `DELETE FROM github_app_deliveries WHERE received_at < ?`, sqliteTime(before))
	return err
}

func scanGHRepo(sc interface{ Scan(...any) error }) (persistence.GitHubAppRepo, error) {
	var r persistence.GitHubAppRepo
	var created, updated string
	if err := sc.Scan(&r.Repo, &r.InstallationID, &r.ProjectID, &r.State, &created, &updated); err != nil {
		return r, err
	}
	r.CreatedAt, _ = time.Parse(persistence.SQLiteTimeLayout, created)
	r.UpdatedAt, _ = time.Parse(persistence.SQLiteTimeLayout, updated)
	return r, nil
}

const ghRepoCols = `repo, installation_id, project_id, state, created_at, updated_at`

func (s ghTx) GetRepo(ctx context.Context, repo string) (*persistence.GitHubAppRepo, error) {
	r, err := scanGHRepo(s.tx.QueryRowContext(ctx, `SELECT `+ghRepoCols+` FROM github_app_repositories WHERE repo = ?`, repo))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s ghTx) ReposOfInstallation(ctx context.Context, id int64) ([]persistence.GitHubAppRepo, error) {
	rows, err := s.tx.QueryContext(ctx, `SELECT `+ghRepoCols+` FROM github_app_repositories WHERE installation_id = ? ORDER BY repo`, id)
	if err != nil {
		return nil, err
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

func (s ghTx) UpsertRepo(ctx context.Context, r persistence.GitHubAppRepo) error {
	_, err := s.tx.ExecContext(ctx, `INSERT INTO github_app_repositories (`+ghRepoCols+`) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo) DO UPDATE SET installation_id = excluded.installation_id, project_id = excluded.project_id,
		state = excluded.state, updated_at = excluded.updated_at`,
		r.Repo, r.InstallationID, r.ProjectID, r.State, sqliteTime(r.CreatedAt), sqliteTime(r.UpdatedAt))
	return err
}

func (s ghTx) InsertRepo(ctx context.Context, r persistence.GitHubAppRepo) (bool, error) {
	res, err := s.tx.ExecContext(ctx, `INSERT INTO github_app_repositories (`+ghRepoCols+`) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo) DO NOTHING`, r.Repo, r.InstallationID, r.ProjectID, r.State, sqliteTime(r.CreatedAt), sqliteTime(r.UpdatedAt))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s ghTx) SetSuspended(ctx context.Context, id int64, suspended bool, now time.Time) error {
	_, err := s.tx.ExecContext(ctx, `INSERT INTO github_app_installations (installation_id, suspended, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (installation_id) DO UPDATE SET suspended = excluded.suspended, updated_at = excluded.updated_at`,
		id, suspended, sqliteTime(now))
	return err
}

// ApplyInstallationEvent implements the interface.
func (r *GitHubAppBindingRepository) ApplyInstallationEvent(ctx context.Context, ev persistence.GitHubAppInstallationEvent, now time.Time) (persistence.GitHubAppEventResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return persistence.GitHubAppEventResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := persistence.ApplyGitHubAppEvent(ctx, ghTx{tx: tx}, ev, now)
	if err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// ListGitHubAppRepos implements the interface.
func (r *GitHubAppBindingRepository) ListGitHubAppRepos(ctx context.Context) ([]persistence.GitHubAppRepo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+ghRepoCols+` FROM github_app_repositories ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.GitHubAppRepo
	for rows.Next() {
		x, err := scanGHRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SuspendedGitHubAppInstallations implements the interface.
func (r *GitHubAppBindingRepository) SuspendedGitHubAppInstallations(ctx context.Context) (map[int64]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT installation_id FROM github_app_installations WHERE suspended`)
	if err != nil {
		return nil, err
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
	for _, f := range from {
		res, err := r.db.ExecContext(ctx, `UPDATE github_app_repositories SET state = ?, updated_at = ? WHERE repo = ? AND state = ?`,
			to, sqliteTime(now), persistence.NormaliseGitHubRepo(repo), f)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return true, nil
		}
	}
	return false, nil
}
