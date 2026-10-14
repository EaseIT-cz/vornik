package persistence

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The ONE definition of an Art 18 restricted memory chunk (GDPR increment 6,
// GitHub #343).
//
// A chunk is restricted while a subject with an ACTIVE restriction
// (data_subject_restrictions.lifted_at IS NULL) is linked to it by EITHER route:
//
//  1. directly — a data_subject_links row (table_name 'project_memory_chunks',
//     row_id = the chunk), or
//  2. through the knowledge graph — the subject is linked to a knowledge_entities
//     row and the chunk MENTIONS that entity (entity_mentions). This is the
//     binder's own resolution rule (design §4.2.1: an entity link plus one link
//     per mentioning chunk), evaluated live, so a chunk ingested AFTER the
//     restriction that mentions the restricted person's entity is restricted
//     from its first read instead of waiting for `subject resolve-kg` to re-run.
//
// Default-deny by construction (security review 2026-10-12): the predicate is a
// NOT EXISTS over positive equi-joins on NOT NULL columns, so no NULL can make it
// pass a row it should exclude — there is no `<>`, no `NOT IN (subquery)`, and an
// active restriction is the NULL `lifted_at` state. If any table it names is
// unreadable, the enclosing query errors and returns nothing.
//
// Every content read embeds ChunkNotRestrictedSQL (enforced by
// memory.TestLaw_EveryChunkContentReadCarriesTheRestrictionPredicate); the
// Searcher's post-fetch screen asks the same question via RestrictedChunkIDsSQL.
// Both are built from restrictedChunkBy, so the two forms cannot drift.
//
// see LLD § https://docs.vornik.io §4.16.1, §4.16.6

// activeRestrictionJoin is the ONE join every restriction rule (chunk and
// entity, predicate and id-set form) is built from: every link of a subject
// whose restriction is active. Changing what "restricted" means is a change
// here, and reaches every rule.
const activeRestrictionJoin = `FROM data_subject_restrictions dsr
      JOIN data_subject_links dsl ON dsl.subject_id = dsr.subject_id
     WHERE dsr.lifted_at IS NULL`

// activeRestrictionLinks is the join as an EXISTS body.
const activeRestrictionLinks = `SELECT 1 ` + activeRestrictionJoin

// restrictedChunkBy returns the EXISTS body for the chunk whose id is idExpr.
func restrictedChunkBy(idExpr string) string {
	return activeRestrictionLinks + `
       AND ((dsl.table_name = 'project_memory_chunks' AND dsl.row_id = ` + idExpr + `)
         OR (dsl.table_name = 'knowledge_entities' AND EXISTS (
               SELECT 1 FROM entity_mentions dsem
                WHERE dsem.chunk_id = ` + idExpr + ` AND dsem.entity_id = dsl.row_id)))`
}

// ChunkNotRestrictedSQL returns a predicate true for a chunk NOT under an active
// restriction. idExpr is the chunk-id expression of the enclosing query, e.g.
// "id", "c.id", "project_memory_chunks.id" or "memory_embed_queue.chunk_id".
//
// An UNQUALIFIED expression is qualified as project_memory_chunks.<col>. This is
// load-bearing, not cosmetic (security review 2026-10-12): inside the subquery a
// bare `id` resolves to data_subject_restrictions.id, so `dsl.row_id = id` compares
// a link with a restriction id, is never true, and the predicate passes EVERY row.
// The first cut of ListChunkContents shipped exactly that.
func ChunkNotRestrictedSQL(idExpr string) string {
	idExpr = strings.TrimSpace(idExpr)
	if idExpr == "" {
		idExpr = "id"
	}
	if !strings.Contains(idExpr, ".") {
		idExpr = "project_memory_chunks." + idExpr
	}
	return "NOT EXISTS (" + restrictedChunkBy(idExpr) + ")"
}

// RestrictedChunkIDsSQL selects which of a set of chunk ids are restricted.
// placeholders is the rendered bind list ("$1,$2" or "?,?").
func RestrictedChunkIDsSQL(placeholders string) string {
	return "SELECT rc.id FROM project_memory_chunks rc WHERE rc.id IN (" + placeholders +
		") AND EXISTS (" + restrictedChunkBy("rc.id") + ")"
}

// --- knowledge-graph entities (GitHub #350, design §4.16.7) -----------------

// restrictedEntityBy returns the EXISTS body for the knowledge entity whose id is
// idExpr: the subject is linked to the entity itself. It is the entity half of
// the chunk rule's route 2, so an entity is restricted exactly when the chunks
// that mention it are restricted through it.
func restrictedEntityBy(idExpr string) string {
	return activeRestrictionLinks + `
       AND dsl.table_name = 'knowledge_entities' AND dsl.row_id = ` + idExpr
}

// EntityNotRestrictedSQL returns a predicate true for a knowledge entity NOT
// under an active restriction. An unqualified idExpr is qualified to
// knowledge_entities for the reason ChunkNotRestrictedSQL gives.
func EntityNotRestrictedSQL(idExpr string) string {
	idExpr = strings.TrimSpace(idExpr)
	if idExpr == "" {
		idExpr = "id"
	}
	if !strings.Contains(idExpr, ".") {
		idExpr = "knowledge_entities." + idExpr
	}
	return "NOT EXISTS (" + restrictedEntityBy(idExpr) + ")"
}

// RestrictedEntityIDsSQL selects which of a set of entity ids are restricted.
// It reads only the subject tables, so it answers for an id whatever state the
// entity row is in. placeholders is the rendered bind list ("$1,$2" or "?,?").
//
// It selects straight from the shared join rather than wrapping
// restrictedEntityBy in an EXISTS: that body re-aliases dsl, so
// restrictedEntityBy("dsl.row_id") would compare the inner alias with itself — a
// tautology that only looks like a guard (design review round 2, N1).
func RestrictedEntityIDsSQL(placeholders string) string {
	return "SELECT DISTINCT dsl.row_id " + activeRestrictionJoin +
		" AND dsl.table_name = 'knowledge_entities' AND dsl.row_id IN (" + placeholders + ")"
}

// RestrictionScreen answers, for a set of ids, which are under an active Art 18
// restriction. The knowledge-graph Searcher requires one and refuses to read
// without it (design §4.16.7). Implementations fail rather than guess: an error
// means "unknown", and the caller withholds.
type RestrictionScreen interface {
	RestrictedEntities(ctx context.Context, entityIDs []string) (map[string]bool, error)
	RestrictedChunks(ctx context.Context, chunkIDs []string) (map[string]bool, error)
}

// ScreenRestrictedEntities drops entities under an active restriction. A nil
// screen or a screen error returns an error and no entities: fail closed.
func ScreenRestrictedEntities(ctx context.Context, screen RestrictionScreen, ents []*KnowledgeEntity) ([]*KnowledgeEntity, error) {
	if screen == nil {
		return nil, ErrNoRestrictionScreen
	}
	if len(ents) == 0 {
		return ents, nil
	}
	ids := make([]string, 0, len(ents))
	for _, e := range ents {
		if e != nil {
			ids = append(ids, e.ID)
		}
	}
	restricted, err := screen.RestrictedEntities(ctx, ids)
	if err != nil {
		return nil, restrictionCheckFailed(err)
	}
	kept := make([]*KnowledgeEntity, 0, len(ents))
	for _, e := range ents {
		if e != nil && !restricted[e.ID] {
			kept = append(kept, e)
		}
	}
	return kept, nil
}

// ScreenRestrictedEdges is the ONE edge rule (design §4.16.7): an edge is
// withheld when EITHER endpoint is restricted, or when it has evidence and EVERY
// source chunk is restricted (derived only from data that may not be used). An
// edge with mixed evidence stays. Fails closed like ScreenRestrictedEntities.
func ScreenRestrictedEdges(ctx context.Context, screen RestrictionScreen, edges []*KnowledgeEdge) ([]*KnowledgeEdge, error) {
	if screen == nil {
		return nil, ErrNoRestrictionScreen
	}
	if len(edges) == 0 {
		return edges, nil
	}
	endpoints := map[string]struct{}{}
	chunks := map[string]struct{}{}
	for _, e := range edges {
		if e == nil {
			continue
		}
		endpoints[e.FromEntity] = struct{}{}
		endpoints[e.ToEntity] = struct{}{}
		for _, c := range e.SourceChunks {
			chunks[c] = struct{}{}
		}
	}
	restrictedEnt, err := screen.RestrictedEntities(ctx, sortedKeys(endpoints))
	if err != nil {
		return nil, restrictionCheckFailed(err)
	}
	restrictedChunk := map[string]bool{}
	if len(chunks) > 0 {
		if restrictedChunk, err = screen.RestrictedChunks(ctx, sortedKeys(chunks)); err != nil {
			return nil, restrictionCheckFailed(err)
		}
	}
	kept := make([]*KnowledgeEdge, 0, len(edges))
	for _, e := range edges {
		if e == nil || restrictedEnt[e.FromEntity] || restrictedEnt[e.ToEntity] {
			continue
		}
		if len(e.SourceChunks) > 0 && allRestricted(e.SourceChunks, restrictedChunk) {
			continue
		}
		kept = append(kept, e)
	}
	return kept, nil
}

// ErrNoRestrictionScreen is returned when a read that must be screened has no
// screen. There is no unscreened mode.
var ErrNoRestrictionScreen = errors.New("no Art 18 restriction screen wired; refusing to read")

func restrictionCheckFailed(err error) error {
	return fmt.Errorf("read withheld because the processing-restriction check failed: %w", err)
}

func allRestricted(ids []string, set map[string]bool) bool {
	for _, id := range ids {
		if !set[id] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SQLRestrictionScreen is the production RestrictionScreen over the daemon's
// handle. The SQL is the shared predicate and runs unchanged on both drivers
// (SQLite carries the two subject tables since design §4.16.6 Shape 5).
type SQLRestrictionScreen struct {
	db DBTX
}

// NewSQLRestrictionScreen builds the screen.
func NewSQLRestrictionScreen(db DBTX) *SQLRestrictionScreen { return &SQLRestrictionScreen{db: db} }

var _ RestrictionScreen = (*SQLRestrictionScreen)(nil)

// restrictionScreenBatch bounds one statement's bind list; both drivers accept
// far more, this only keeps a pathological edge set from building one huge IN.
const restrictionScreenBatch = 500

// RestrictedEntities implements RestrictionScreen.
func (s *SQLRestrictionScreen) RestrictedEntities(ctx context.Context, ids []string) (map[string]bool, error) {
	return s.restricted(ctx, ids, RestrictedEntityIDsSQL, "entity")
}

// RestrictedChunks implements RestrictionScreen.
func (s *SQLRestrictionScreen) RestrictedChunks(ctx context.Context, ids []string) (map[string]bool, error) {
	return s.restricted(ctx, ids, RestrictedChunkIDsSQL, "chunk")
}

func (s *SQLRestrictionScreen) restricted(ctx context.Context, ids []string, query func(string) string, what string) (map[string]bool, error) {
	out := map[string]bool{}
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("restriction screen: no database handle")
	}
	for start := 0; start < len(ids); start += restrictionScreenBatch {
		end := start + restrictionScreenBatch
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			ph[i] = fmt.Sprintf("$%d", i+1)
			args[i] = id
		}
		if err := func() error {
			rows, err := s.db.QueryContext(ctx, query(strings.Join(ph, ",")), args...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				out[id] = true
			}
			return rows.Err()
		}(); err != nil {
			return nil, fmt.Errorf("restriction screen: restricted-%s lookup: %w", what, err)
		}
	}
	return out, nil
}
