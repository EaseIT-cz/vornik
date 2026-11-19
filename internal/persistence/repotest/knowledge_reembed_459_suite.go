package repotest

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #459 (knowledge-graph design, amendment 2026-11-18): an entity
// stored with no vector during an embedder outage is found and filled.
// Other tests may share the database, so the cases filter by their own two
// projects.
func runKnowledgeEntityReembedCases(t *testing.T, repo persistence.KnowledgeEntityRepository) {
	t.Helper()
	ctx := context.Background()
	p1, p2 := uniqueID("proj-reembed-a"), uniqueID("proj-reembed-b")
	mk := func(project, name, lifecycle string, vec []float32) *persistence.KnowledgeEntity {
		e := &persistence.KnowledgeEntity{ProjectID: project, Type: "person", CanonicalName: name, LifecycleState: lifecycle, Embedding: vec}
		if err := repo.Insert(ctx, e); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return e
	}
	// Postgres stores vector(1024); a shorter vector is refused there.
	vec := func(first float32) []float32 {
		v := make([]float32, 1024)
		v[0] = first
		return v
	}
	a := mk(p1, "No Vector A", "published", nil)
	b := mk(p2, "No Vector B", "staged", nil)
	mk(p1, "Has Vector", "published", vec(0.1))
	mk(p1, "Superseded No Vector", "superseded", nil)

	t.Run("ListMissingEmbedding_finds_vectorless_live_entities_across_projects_459", func(t *testing.T) {
		got, err := repo.ListMissingEmbedding(ctx, 0) // unbounded: other tests may share the database
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, e := range got {
			if e.ProjectID == p1 || e.ProjectID == p2 {
				seen[e.CanonicalName] = true
			}
		}
		if !seen["No Vector A"] || !seen["No Vector B"] || seen["Has Vector"] || seen["Superseded No Vector"] {
			t.Fatalf("listed %v, want exactly the two vectorless, not superseded entities of both projects", seen)
		}
		if one, err := repo.ListMissingEmbedding(ctx, 1); err != nil || len(one) != 1 {
			t.Fatalf("limit 1 returned %d (%v)", len(one), err)
		}
	})
	t.Run("UpdateEmbedding_fills_only_a_missing_vector_459", func(t *testing.T) {
		if err := repo.UpdateEmbedding(ctx, a.ID, vec(0.5)); err != nil {
			t.Fatal(err)
		}
		if err := repo.UpdateEmbedding(ctx, a.ID, vec(0.9)); err != nil {
			t.Fatal(err)
		}
		got, err := repo.Get(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Embedding) != 1024 || got.Embedding[0] != 0.5 {
			t.Fatalf("embedding = %v, want the first write kept", got.Embedding)
		}
		left, _ := repo.ListMissingEmbedding(ctx, 0)
		for _, e := range left {
			if e.ID == a.ID {
				t.Fatal("a filled entity is still listed")
			}
		}
		_ = b
	})
}
