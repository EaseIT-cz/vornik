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
	return r.substringSearch(ctx, projectID, query, limit, ScopeFilter{RepoScope: "", Strict: false})
}

// SubstringSearchTemporalForTest calls the temporal substring fallback.
func SubstringSearchTemporalForTest(ctx context.Context, r *Repository, projectID, query string, limit int, from time.Time) ([]SearchResult, error) {
	return r.substringSearchTemporal(ctx, projectID, query, limit, from, time.Time{}, ScopeFilter{RepoScope: "", Strict: false})
}

// GitHub #368: the scoped substring fallbacks, reached in production only
// when their FTS statement errors, called directly so the scope filter and
// the repo_scope projection are asserted on the real database.

// SubstringSearchScopedForTest calls the unscoped-cascade substring fallback
// (S9) with an explicit scope.
func SubstringSearchScopedForTest(ctx context.Context, r *Repository, projectID, query string, limit int, repoScope string, strict bool) ([]SearchResult, error) {
	return r.substringSearch(ctx, projectID, query, limit, ScopeFilter{RepoScope: repoScope, Strict: strict})
}

// SubstringSearchTemporalScopedForTest calls S6 with a scope.
func SubstringSearchTemporalScopedForTest(ctx context.Context, r *Repository, projectID, query string, limit int, repoScope string, strict bool) ([]SearchResult, error) {
	return r.substringSearchTemporal(ctx, projectID, query, limit, time.Time{}, time.Time{}, ScopeFilter{RepoScope: repoScope, Strict: strict})
}

// SubstringSearchWithEpochsScopedForTest calls S3 with a scope.
func SubstringSearchWithEpochsScopedForTest(ctx context.Context, r *Repository, projectID, query string, limit int, repoScope string, strict bool) ([]SearchResult, error) {
	return r.substringSearchWithEpochsTemporal(ctx, projectID, query, limit, nil, time.Time{}, time.Time{}, ScopeFilter{RepoScope: repoScope, Strict: strict})
}

// SubstringSearchFilteredForTest calls S9 with a full ScopeFilter (#550).
func SubstringSearchFilteredForTest(ctx context.Context, r *Repository, projectID, query string, limit int, sf ScopeFilter) ([]SearchResult, error) {
	return r.substringSearch(ctx, projectID, query, limit, sf)
}

// SubstringSearchTemporalFilteredForTest calls S6 with a full ScopeFilter (#550).
func SubstringSearchTemporalFilteredForTest(ctx context.Context, r *Repository, projectID, query string, limit int, sf ScopeFilter) ([]SearchResult, error) {
	return r.substringSearchTemporal(ctx, projectID, query, limit, time.Time{}, time.Time{}, sf)
}

// SubstringSearchWithEpochsFilteredForTest calls S3 with a full ScopeFilter (#550).
func SubstringSearchWithEpochsFilteredForTest(ctx context.Context, r *Repository, projectID, query string, limit int, sf ScopeFilter) ([]SearchResult, error) {
	return r.substringSearchWithEpochsTemporal(ctx, projectID, query, limit, nil, time.Time{}, time.Time{}, sf)
}
