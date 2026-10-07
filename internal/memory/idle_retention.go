package memory

import (
	"context"
	"fmt"
	"github.com/lib/pq"
)

// SetApprovalRetentionResolver reads current project policy. Wire before serving;
// registry reloads are observed by the callback without mutating this repository.
func (r *Repository) SetApprovalRetentionResolver(fn func(string) bool) {
	r.approvalRetention = fn
}

func (r *Repository) approvalRetentionEnabled(projectID string) bool {
	return r != nil && r.approvalRetention != nil && r.approvalRetention(projectID)
}

// expiryClause owns TTL availability for every retrieval query. Other lifecycle,
// firewall and scope predicates remain independent of idle retention.
func (r *Repository) expiryClause(projectID, alias string) string {
	if r.approvalRetentionEnabled(projectID) {
		return "TRUE"
	}
	return "(" + alias + "expires_at IS NULL OR " + alias + "expires_at > NOW())"
}

// recordReturnedUse counts only the post-filter selected set. UPDATE locks the
// same rows as approval deletion. If deletion won, its missing rows cannot be
// returned to the caller as if their use had been recorded.
func (s *Searcher) recordReturnedUse(ctx context.Context, projectID string, hits []SearchResult) ([]SearchResult, error) {
	if s.repo == nil || !s.repo.approvalRetentionEnabled(projectID) || len(hits) == 0 {
		return hits, nil
	}
	ids := make([]string, 0, len(hits))
	contents := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ChunkID)
		contents = append(contents, hit.Content)
	}
	rows, err := s.repo.db.QueryContext(ctx, `UPDATE project_memory_chunks SET last_used_at=GREATEST(COALESCE(last_used_at,created_at),clock_timestamp()) FROM unnest($2::text[], $3::text[]) AS selected(id,content) WHERE project_id=$1 AND project_memory_chunks.id=selected.id AND project_memory_chunks.content=selected.content RETURNING project_memory_chunks.id`, projectID, pq.Array(ids), pq.Array(contents))
	if err != nil {
		return nil, fmt.Errorf("record returned memory use: %w", err)
	}
	defer func() { _ = rows.Close() }()
	present := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		present[id] = true
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(hits))
	for _, hit := range hits {
		if present[hit.ChunkID] {
			out = append(out, hit)
		}
	}
	return out, nil
}
