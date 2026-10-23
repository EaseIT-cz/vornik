package sqlite

import "vornik.io/vornik/internal/persistence"

// NewMemoryVerificationRepository is the SQLite handle on the one
// memory-verification implementation (design 22, amendment #482); SQLite
// stores its timestamps as fixed-width text.
func NewMemoryVerificationRepository(db persistence.DBTX) *persistence.SQLMemoryVerificationRepository {
	return persistence.NewSQLMemoryVerificationRepository(db, true)
}
