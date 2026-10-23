package postgres

import (
	"database/sql"
	"strings"
	"time"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence/subjectstore"
)

// DataSubjectRepository persists the GDPR data-subject axis, the rights ledger
// and the Art 18 restriction record on Postgres.
//
// It is the driver-neutral subjectstore.Repository with the Postgres dialect:
// one implementation serves both drivers since GitHub #355 (2026-10-13), so the
// safety checks it carries cannot diverge between them. The alias keeps every
// caller that names *postgres.DataSubjectRepository unchanged.
//
// see LLD § https://docs.vornik.io §4.19
type DataSubjectRepository = subjectstore.Repository

// SubjectStoreDialect is the Postgres side of subjectstore.Dialect: time.Time
// bound and scanned natively, pq error codes mapped by mapDBError, and both
// capabilities on (Postgres stores channel sessions and the embedding
// cache, keyed by memory.EmbedInputHash).
func SubjectStoreDialect() subjectstore.Dialect {
	return subjectstore.Dialect{
		Name:            "postgres",
		MapErr:          mapDBError,
		EmbedCacheKey:   memory.EmbedInputHash,
		ChannelSessions: true,
		// blackbox_trace_cache (migration 68): an erasure evicts it
		// (Art 17 design §15.12, GitHub #415 item 3).
		TraceCache: true,
	}
}

// NewDataSubjectRepository constructs the repository.
func NewDataSubjectRepository(db *sql.DB) *DataSubjectRepository {
	return subjectstore.New(db, SubjectStoreDialect())
}

// nullIfEmpty binds a blank string as NULL.
func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// nullTimeIfZero binds a zero time as NULL.
func nullTimeIfZero(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
