package postgres

import "vornik.io/vornik/internal/persistence"

// NewMemoryVerificationRepository is the Postgres handle on the one
// memory-verification implementation (design 22, amendment #482).
func NewMemoryVerificationRepository(db persistence.DBTX) *persistence.SQLMemoryVerificationRepository {
	return persistence.NewSQLMemoryVerificationRepository(db, false)
}
