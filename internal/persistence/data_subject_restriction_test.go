package persistence

import (
	"regexp"
	"strings"
	"testing"
)

// audit 2026-10-12 (security review of #343): the Art 18 predicate must be
// default-deny. A `<>` or a `NOT IN (subquery)` passes a row whenever one side is
// NULL; this predicate is NOT EXISTS over positive equi-joins only, and these
// tests fail if a later edit reintroduces either shape.
func TestChunkNotRestrictedSQL_HasNoNullUnsafeShapes(t *testing.T) {
	for _, q := range []string{ChunkNotRestrictedSQL("c.id"), RestrictedChunkIDsSQL("$1")} {
		if strings.Contains(q, "<>") || strings.Contains(q, "!=") {
			t.Errorf("inequality in the restriction predicate: %s", q)
		}
		if regexp.MustCompile(`(?i)NOT\s+IN`).MatchString(q) {
			t.Errorf("NOT IN in the restriction predicate: %s", q)
		}
		if !strings.Contains(q, "dsr.lifted_at IS NULL") {
			t.Errorf("an active restriction must be the NULL lifted_at state: %s", q)
		}
	}
	if !strings.HasPrefix(ChunkNotRestrictedSQL("c.id"), "NOT EXISTS (") {
		t.Error("the use-path predicate must be a NOT EXISTS")
	}
}

// Identity (review shape 4): a chunk that merely MENTIONS a restricted subject's
// knowledge-graph entity is restricted too, without waiting for a re-run binder.
func TestChunkNotRestrictedSQL_CoversTheEntityMentionRoute(t *testing.T) {
	q := ChunkNotRestrictedSQL("c.id")
	for _, want := range []string{
		"dsl.table_name = 'project_memory_chunks' AND dsl.row_id = c.id",
		"dsl.table_name = 'knowledge_entities'",
		"FROM entity_mentions dsem",
		"dsem.chunk_id = c.id AND dsem.entity_id = dsl.row_id",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("predicate lacks %q:\n%s", want, q)
		}
	}
}

// audit 2026-10-12 (security review of #343): a bare `id` inside the subquery
// binds to data_subject_restrictions.id, so the predicate compared a link with a
// restriction id and passed every row — ListChunkContents shipped that way. Every
// unqualified expression must come out qualified to the chunk table.
func TestChunkNotRestrictedSQL_NeverEmitsABareId(t *testing.T) {
	for _, in := range []string{"", "id", " id "} {
		q := ChunkNotRestrictedSQL(in)
		if regexp.MustCompile(`=\s*id\b`).MatchString(q) {
			t.Errorf("ChunkNotRestrictedSQL(%q) compares against a bare id:\n%s", in, q)
		}
		if !strings.Contains(q, "dsl.row_id = project_memory_chunks.id") {
			t.Errorf("ChunkNotRestrictedSQL(%q) is not qualified to the chunk table:\n%s", in, q)
		}
	}
	if !strings.Contains(ChunkNotRestrictedSQL("memory_embed_queue.chunk_id"), "= memory_embed_queue.chunk_id") {
		t.Error("a qualified expression must pass through unchanged")
	}
}

// audit 2026-10-12 (GitHub #350): an Art 18 restriction must cover the subject's
// linked knowledge-graph ENTITY, by the same NULL-safe shape as the chunk rule,
// built from the same join so the two cannot drift.
func TestEntityRestrictionSQL_IsTheChunkRulesEntityHalf(t *testing.T) {
	for _, q := range []string{EntityNotRestrictedSQL("ke.id"), RestrictedEntityIDsSQL("$1,$2")} {
		if strings.Contains(q, "<>") || strings.Contains(q, "!=") || regexp.MustCompile(`(?i)NOT\s+IN`).MatchString(q) {
			t.Errorf("NULL-unsafe shape in the entity predicate: %s", q)
		}
		if !strings.Contains(q, activeRestrictionJoin) {
			t.Errorf("entity predicate not built from the shared join:\n%s", q)
		}
		if !strings.Contains(q, "dsl.table_name = 'knowledge_entities'") {
			t.Errorf("entity predicate does not name the entity link:\n%s", q)
		}
	}
	if !strings.Contains(ChunkNotRestrictedSQL("c.id"), activeRestrictionJoin) ||
		!strings.Contains(RestrictedChunkIDsSQL("$1"), activeRestrictionJoin) {
		t.Error("chunk predicate not built from the shared join")
	}
	if !strings.HasPrefix(EntityNotRestrictedSQL("ke.id"), "NOT EXISTS (") {
		t.Error("the use-path entity predicate must be a NOT EXISTS")
	}
	for _, in := range []string{"", "id"} {
		if !strings.Contains(EntityNotRestrictedSQL(in), "dsl.row_id = knowledge_entities.id") {
			t.Errorf("EntityNotRestrictedSQL(%q) is not qualified to the entity table", in)
		}
	}
}
