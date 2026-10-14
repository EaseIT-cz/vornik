package persistence

import "time"

// SQLiteTimeLayout is the ONE text form of a SQLite timestamp: UTC, nine
// fractional digits, "Z" — fixed width, so string order is time order. SQLite
// stores timestamps as TEXT and compares them as strings, so every timestamp a
// statement writes or binds against a SQLite column must be in this form.
//
// It lives here, not in internal/persistence/sqlite, because a package outside
// the driver also binds thresholds against SQLite columns: the retention
// sweeper (GitHub #360). One layout, one formatter; the SQLite package's
// sqliteTime delegates to SQLiteTime.
//
// see LLD § https://docs.vornik.io D1
const SQLiteTimeLayout = "2006-01-02T15:04:05.000000000Z"

// SQLiteTime renders t in SQLiteTimeLayout.
func SQLiteTime(t time.Time) string {
	return t.UTC().Format(SQLiteTimeLayout)
}
