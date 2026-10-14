package sqlite

import (
	"database/sql"
	"time"

	"vornik.io/vornik/internal/persistence/subjectstore"
)

// The GDPR data-subject store, the rights ledger and the Art 18 restriction
// record on SQLite (GitHub #355, 2026-10-13). The implementation is the shared
// subjectstore.Repository — the same one Postgres runs — with this dialect.
//
// see LLD § https://docs.vornik.io §4.19

// SubjectStoreDialect is the SQLite side of subjectstore.Dialect: timestamps in
// the one fixed-width text form (sqliteTime / sqlTime, the 2026-10-01 timestamp
// design), the identity repository's error mapping, and no embedding cache or
// stored channel sessions — this driver has neither.
func SubjectStoreDialect() subjectstore.Dialect {
	return subjectstore.Dialect{
		Name:     "sqlite",
		BindTime: func(t time.Time) any { return sqliteTime(t) },
		ParseTime: func(src any) (time.Time, error) {
			var v sqlTime
			err := v.Scan(src)
			return v.Time, err
		},
		MapErr:          mapIdentityDBError,
		EmbedCacheKey:   nil,
		ChannelSessions: false,
	}
}

// NewDataSubjectRepository builds the shared repository over a SQLite handle.
func NewDataSubjectRepository(db *sql.DB) *subjectstore.Repository {
	return subjectstore.New(db, SubjectStoreDialect())
}
