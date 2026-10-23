package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// TestMemoryEntities_WildcardQueryListsNothing_449 drives the entity browser
// (/ui/memory/<p>/entities?q=) over a real SQLite database and a real
// graph.Searcher. GitHub #449 (security audit 2026-10-17): the search box's
// query reached KnowledgeEntityFilter.NameLike as a LIKE pattern, so q=_ or
// q=% listed every entity of the project, and the case rule differed per
// driver. The rule is now a literal substring, ASCII-case-insensitive.
func TestMemoryEntities_WildcardQueryListsNothing_449(t *testing.T) {
	db := sqlitetest.Memory(t)
	entities := sqlite.NewKnowledgeEntityRepository(db.DB)
	ctx := context.Background()
	for _, n := range []string{"ACME Blinds", "Velux Czechia", "a_b corp", "100% Beef"} {
		require.NoError(t, entities.Insert(ctx, &persistence.KnowledgeEntity{
			ProjectID: "p1", Type: "VENDOR", CanonicalName: n,
		}))
	}
	searcher := graph.NewSearcher(entities, sqlite.NewKnowledgeEdgeRepository(db.DB),
		sqlite.NewEntityMentionRepository(db.DB), nil, nil)
	searcher.SetRestrictionScreen(persistence.NewSQLRestrictionScreen(db.DB))

	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	srv.kgSearcher = searcher

	get := func(q string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/memory/p1/entities?q="+url.QueryEscape(q), nil)
		rec := httptest.NewRecorder()
		srv.memoryRouter(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Body.String()
	}

	body := get("_")
	assert.Contains(t, body, "a_b corp")
	assert.NotContains(t, body, "ACME Blinds", "q=_ must not match every entity")
	assert.NotContains(t, body, "Velux Czechia", "q=_ must not match every entity")

	body = get("%")
	assert.Contains(t, body, "100% Beef")
	assert.NotContains(t, body, "ACME Blinds", "q=% must not match every entity")

	body = get("acme")
	assert.Contains(t, body, "ACME Blinds", "ASCII case still folds")
	assert.NotContains(t, body, "Velux Czechia")
}
