package repotest

import (
	"context"
	"database/sql"
	"time"
)

// DBExec is the SQLExec the driver tests hand the transcript suite: the
// handle, and the driver's timestamp binding.
type DBExec struct {
	DB       *sql.DB
	BindTime func(time.Time) any
}

// Exec runs a statement.
func (d DBExec) Exec(ctx context.Context, query string, args ...any) error {
	_, err := d.DB.ExecContext(ctx, query, args...)
	return err
}

// QueryString reads one nullable text value; nil for NULL or no row.
func (d DBExec) QueryString(ctx context.Context, query string, args ...any) (*string, error) {
	var v sql.NullString
	if err := d.DB.QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if !v.Valid {
		return nil, nil
	}
	return &v.String, nil
}

// Time binds a timestamp in the driver's form.
func (d DBExec) Time(t time.Time) any {
	if d.BindTime == nil {
		return t
	}
	return d.BindTime(t)
}
