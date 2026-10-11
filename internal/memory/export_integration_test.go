//go:build integration

package memory

import (
	"context"
	"time"
)

// Test-only access for the external integration package to the two
// substring fallbacks, which production reaches only when FTS errors
// (GitHub #94: their validation filter is asserted behaviourally).

// SubstringSearchForTest calls the unscoped substring fallback.
func SubstringSearchForTest(ctx context.Context, r *Repository, projectID, query string, limit int) ([]SearchResult, error) {
	return r.substringSearch(ctx, projectID, query, limit, "", false)
}

// SubstringSearchTemporalForTest calls the temporal substring fallback.
func SubstringSearchTemporalForTest(ctx context.Context, r *Repository, projectID, query string, limit int, from time.Time) ([]SearchResult, error) {
	return r.substringSearchTemporal(ctx, projectID, query, limit, from, time.Time{}, "", false)
}
