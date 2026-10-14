package memory

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// GDPR Art 18 restriction of processing — the memory-side screen (increment 6,
// GitHub #343).
//
// A chunk linked to a subject under an active restriction is stored but not USED.
// The screen runs at the two seams every use path ends in:
//
//   - inside searchInternal, straight after the single candidate fetch and BEFORE
//     the LLM reranker — the firewall runs after the reranker, so a screen there
//     would stop restricted data being served while still sending it to a model;
//   - at the top of firewallListedRows, the seam every listing and export uses.
//
// It is NOT a firewall decision: it runs whether or not a firewall is wired,
// ignores the enforcement mode, and fails CLOSED. The question it asks is the one
// persistence.RestrictedChunkIDsSQL defines.
//
// see LLD § https://docs.vornik.io §4.16.1

// RestrictionScreen reports which of a set of chunk ids are under an active
// Art 18 restriction.
type RestrictionScreen interface {
	RestrictedChunks(ctx context.Context, chunkIDs []string) (map[string]bool, error)
}

// SetRestrictionScreen installs the screen. memory.New installs the repository;
// a Searcher built by NewSearcher alone (tests) has none.
func (s *Searcher) SetRestrictionScreen(r RestrictionScreen) {
	if s != nil {
		s.restriction = r
	}
}

// RestrictedChunks implements RestrictionScreen over project_memory_chunks.
func (r *Repository) RestrictedChunks(ctx context.Context, chunkIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(chunkIDs) == 0 {
		return out, nil
	}
	ph := make([]string, len(chunkIDs))
	args := make([]any, len(chunkIDs))
	for i, id := range chunkIDs {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	rows, err := r.db.QueryContext(ctx, persistence.RestrictedChunkIDsSQL(strings.Join(ph, ",")), args...)
	if err != nil {
		return nil, fmt.Errorf("memory: restricted-chunk lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("memory: scan restricted chunk: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate restricted chunks: %w", err)
	}
	return out, nil
}

// screenRestrictedResults drops restricted chunks from a recall candidate set.
// An error is returned rather than the unfiltered set: a recall that fails is
// visible, a recall that silently used restricted data is a breach.
func (s *Searcher) screenRestrictedResults(ctx context.Context, projectID string, results []SearchResult) ([]SearchResult, error) {
	if s.restriction == nil || len(results) == 0 {
		return results, nil
	}
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.ChunkID
	}
	restricted, err := s.restriction.RestrictedChunks(ctx, ids)
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).
			Msg("memory: Art 18 restriction screen failed — withholding the whole recall")
		return nil, fmt.Errorf("memory: recall withheld because the processing-restriction check failed: %w", err)
	}
	if len(restricted) == 0 {
		return results, nil
	}
	kept := results[:0:0]
	for _, r := range results {
		if !restricted[r.ChunkID] {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// screenRestrictedRows is the listing-seam twin. Listings return no error, so a
// failed lookup withholds every row and logs.
func (s *Searcher) screenRestrictedRows(ctx context.Context, projectID string, rows []RecentChunkRow) []RecentChunkRow {
	if s.restriction == nil || len(rows) == 0 {
		return rows
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ChunkID
	}
	restricted, err := s.restriction.RestrictedChunks(ctx, ids)
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).
			Msg("memory: Art 18 restriction screen failed — withholding the whole listing")
		return nil
	}
	if len(restricted) == 0 {
		return rows
	}
	kept := rows[:0:0]
	for _, r := range rows {
		if !restricted[r.ChunkID] {
			kept = append(kept, r)
		}
	}
	return kept
}
