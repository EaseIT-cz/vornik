package service

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/storage"
)

func TestNewGraphSearcher_NilWhenUnwired(t *testing.T) {
	// No repos, no DB → nil.
	c := &Container{}
	if got := c.newGraphSearcher(); got != nil {
		t.Fatal("expected nil searcher with no repos/DB")
	}

	// repos present but DB nil → nil.
	c2 := &Container{repos: &storage.Repositories{}}
	if got := c2.newGraphSearcher(); got != nil {
		t.Fatal("expected nil searcher with nil DB")
	}

	// DB present but KG repos nil → nil.
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	c3 := &Container{DB: db, repos: &storage.Repositories{}}
	if got := c3.newGraphSearcher(); got != nil {
		t.Fatal("expected nil searcher with nil KG repos")
	}
}

func TestNewGraphSearcher_BuildsWhenWired(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	repos := &storage.Repositories{
		KnowledgeEntities: postgres.NewKnowledgeEntityRepository(db),
		KnowledgeEdges:    postgres.NewKnowledgeEdgeRepository(db),
		EntityMentions:    postgres.NewEntityMentionRepository(db),
	}

	// Without a memory manager → builds with a nil embedder.
	c := &Container{DB: db, repos: repos}
	if got := c.newGraphSearcher(); got == nil {
		t.Fatal("expected non-nil searcher when repos + DB are wired (no embedder)")
	}

	// With a memory manager that has an Embedder → the embed closure
	// branch runs.
	c2 := &Container{
		DB:            db,
		repos:         repos,
		memoryManager: &memory.Manager{Embedder: memory.NewEmbedder(memory.Config{})},
	}
	if got := c2.newGraphSearcher(); got == nil {
		t.Fatal("expected non-nil searcher with embedder wired")
	}
}

// audit 2026-10-12 (GitHub #350): the graph Searcher refuses every read without
// an Art 18 restriction screen, so the production constructor must install one —
// a forgotten setter would turn the UI KG pages and the memory_search graph block
// dark, and this pins that it cannot be forgotten.
func TestNewGraphSearcher_InstallsTheRestrictionScreen(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	c := &Container{DB: db, repos: &storage.Repositories{
		KnowledgeEntities: postgres.NewKnowledgeEntityRepository(db),
		KnowledgeEdges:    postgres.NewKnowledgeEdgeRepository(db),
		EntityMentions:    postgres.NewEntityMentionRepository(db),
	}}
	gs := c.newGraphSearcher()
	if gs == nil || !gs.HasRestrictionScreen() {
		t.Fatal("newGraphSearcher must install the Art 18 restriction screen")
	}
}
