package postgres

import (
	"database/sql"

	"vornik.io/vornik/internal/persistence/subjectstore"
)

// IncidentRepository persists the Art 33/34 breach ledger.
//
// It is the driver-neutral subjectstore.IncidentRepository with the Postgres
// dialect: one implementation serves both drivers since GitHub #475
// (2026-11-23). The alias keeps every caller that names
// *postgres.IncidentRepository unchanged.
//
// see LLD § https://docs.vornik.io §4.10, §4.21
type IncidentRepository = subjectstore.IncidentRepository

// NewIncidentRepository constructs the repository.
func NewIncidentRepository(db *sql.DB) *IncidentRepository {
	return subjectstore.NewIncidentRepository(db, SubjectStoreDialect())
}
