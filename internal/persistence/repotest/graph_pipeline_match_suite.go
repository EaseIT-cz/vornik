package repotest

import (
	"context"
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #419 (knowledge-graph LLD §3.3c, found in production): the KG worker
// quarantined 402 chunks with `insert mention for entity ... not found` and 13
// with `upsert edge ...: not found` (2026-10-17 census). `not found` is the
// Postgres driver's mapping of a foreign-key violation: the entity id did not
// exist. The id was the resolver model's `match_id`, copied into the pipeline
// unverified; the edge-class ids read back from production are real catalog
// ids with digits dropped or changed. On SQLite (foreign keys off) the same
// reply stores a mention and an edge pointing at nothing.
//
// This suite drives the real pipeline over the driver's real repositories with
// a scripted resolver reply that names an entity that is not in the project,
// and requires the chunk to complete with every stored link pointing at a real
// entity of the chunk's project.

// RunGraphPipelineResolverMatchSuite pins GitHub #419 on one driver.
func RunGraphPipelineResolverMatchSuite(t *testing.T, repos GraphPipelineRepos, seed SeedChunk) {
	t.Helper()
	t.Run("match_id is a catalog id with a digit dropped", func(t *testing.T) {
		runDamagedMatchIDCase(t, repos, seed)
	})
	t.Run("match with an empty match_id", func(t *testing.T) {
		runEmptyMatchIDCase(t, repos, seed)
	})
	t.Run("match_id of another project's entity", func(t *testing.T) {
		runForeignMatchIDCase(t, repos, seed)
	})
	t.Run("match_id of a real entity of the project outside the catalog shown", func(t *testing.T) {
		runRealMatchIDOutsideCatalogCase(t, repos, seed)
	})
}

// matchCase is the shared shape: a chunk naming "Blinds" (a VENDOR) and
// "Praha" (a LOCATION), an edge between them, and a resolver reply that
// matches the vendor to matchID and makes Praha new.
func runMatchCase(ctx context.Context, t *testing.T, repos GraphPipelineRepos, seed SeedChunk,
	project, chunkID, matchID string,
) error {
	t.Helper()
	content := "Blinds delivered the order to Praha on Monday."
	cands := []kgCand{
		{Type: persistence.EntityTypeVendor, Name: "Blinds"},
		{Type: persistence.EntityTypeLocation, Name: "Praha"},
	}
	type decision struct {
		CandidateID string `json:"candidate_id"`
		Decision    string `json:"decision"`
		MatchID     string `json:"match_id,omitempty"`
	}
	reply, err := json.Marshal([]decision{
		{CandidateID: "cand-0", Decision: "match", MatchID: matchID},
		{CandidateID: "cand-1", Decision: "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	edge := kgEdge{From: "Blinds", To: "Praha", Evidence: "delivered the order to Praha"}
	_, err = runKGChunk(ctx, t, repos, seed, project, chunkID, content, cands, string(reply), edge)
	return err
}

// seedVendor stores the catalog entity the resolver is shown for "Blinds":
// NameLike matches it, and its name is far enough from "Blinds"
// (Levenshtein > 3) that the string short-circuit does not fire, so the
// model's reply is what decides.
func seedVendor(ctx context.Context, t *testing.T, repos GraphPipelineRepos, project string) *persistence.KnowledgeEntity {
	t.Helper()
	e := &persistence.KnowledgeEntity{
		ProjectID: project, Type: persistence.EntityTypeVendor, CanonicalName: "ACME Blinds Incorporated",
	}
	if err := repos.Entities.Insert(ctx, e); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	return e
}

// assertLinksReal requires every mention of the chunk and every edge of the
// project to point at an entity that exists in the project, and returns the
// mentions keyed by entity id.
func assertLinksReal(ctx context.Context, t *testing.T, repos GraphPipelineRepos, project, chunkID string) map[string]*persistence.KnowledgeEntity {
	t.Helper()
	mentions, err := repos.Mentions.ListByChunk(ctx, chunkID)
	if err != nil {
		t.Fatalf("ListByChunk: %v", err)
	}
	out := map[string]*persistence.KnowledgeEntity{}
	for _, m := range mentions {
		e, err := repos.Entities.Get(ctx, m.EntityID)
		if err != nil {
			t.Errorf("mention of chunk %s points at entity %q, which does not exist (#419): %v",
				chunkID, m.EntityID, err)
			continue
		}
		if e.ProjectID != project {
			t.Errorf("mention of chunk %s points at entity %q of project %q, not %q (#419)",
				chunkID, m.EntityID, e.ProjectID, project)
			continue
		}
		out[e.ID] = e
	}
	edges, err := repos.Edges.List(ctx, persistence.KnowledgeEdgeFilter{ProjectID: project})
	if err != nil {
		t.Fatalf("List edges: %v", err)
	}
	for _, ed := range edges {
		for _, id := range []string{ed.FromEntity, ed.ToEntity} {
			if e, err := repos.Entities.Get(ctx, id); err != nil || e.ProjectID != project {
				t.Errorf("edge %s endpoint %q is not an entity of project %q (#419): %v", ed.ID, id, project, err)
			}
		}
	}
	return out
}

// The production shape: the model copied a catalog id and dropped a digit.
// Before the fix: Postgres failed the chunk with `insert mention for entity
// "Blinds" in chunk ...: not found`; SQLite stored the mention and the edge
// pointing at the damaged id.
func runDamagedMatchIDCase(t *testing.T, repos GraphPipelineRepos, seed SeedChunk) {
	ctx := context.Background()
	project, chunkID := uniqueID("proj"), uniqueID("chunk")
	seeded := seedVendor(ctx, t, repos, project)
	damaged := seeded.ID[:len(seeded.ID)-1]

	if err := runMatchCase(ctx, t, repos, seed, project, chunkID, damaged); err != nil {
		t.Fatalf("RunChunk failed the chunk (#419: a resolver match_id that names no entity "+
			"must not fail extraction): %v", err)
	}
	linked := assertLinksReal(ctx, t, repos, project, chunkID)
	if len(linked) != 2 {
		t.Errorf("chunk links %d entities, want 2 (Blinds and Praha)", len(linked))
	}
	// Downgraded to ambiguous: the candidate lands as a quarantined entity
	// under its own name, not as an alias of a guessed row.
	blinds, err := repos.Entities.GetByCanonical(ctx, project, persistence.EntityTypeVendor, "Blinds")
	if err != nil {
		t.Fatalf("the candidate of a rejected match must be stored (as ambiguous): %v", err)
	}
	if blinds.LifecycleState != "quarantined" {
		t.Errorf("rejected match stored with lifecycle %q, want quarantined (ambiguous)", blinds.LifecycleState)
	}
	if _, ok := linked[blinds.ID]; !ok {
		t.Errorf("the chunk's mention is not linked to the ambiguous entity %s", blinds.ID)
	}
	if _, ok := linked[seeded.ID]; ok {
		t.Errorf("a damaged id must not be repaired to the catalog row it resembles")
	}
}

// A match with no id wrote no entity, mention or edge for the candidate and
// counted it as matched.
func runEmptyMatchIDCase(t *testing.T, repos GraphPipelineRepos, seed SeedChunk) {
	ctx := context.Background()
	project, chunkID := uniqueID("proj"), uniqueID("chunk")
	seedVendor(ctx, t, repos, project)

	if err := runMatchCase(ctx, t, repos, seed, project, chunkID, ""); err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	linked := assertLinksReal(ctx, t, repos, project, chunkID)
	if len(linked) != 2 {
		t.Errorf("chunk links %d entities, want 2: a match with an empty match_id silently "+
			"lost the candidate (#419)", len(linked))
	}
}

// An id that exists, but in another project: the foreign key is satisfied, so
// neither driver refused it, and the chunk was linked across projects.
func runForeignMatchIDCase(t *testing.T, repos GraphPipelineRepos, seed SeedChunk) {
	ctx := context.Background()
	project, chunkID := uniqueID("proj"), uniqueID("chunk")
	seedVendor(ctx, t, repos, project)
	foreign := seedVendor(ctx, t, repos, uniqueID("other"))

	if err := runMatchCase(ctx, t, repos, seed, project, chunkID, foreign.ID); err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	linked := assertLinksReal(ctx, t, repos, project, chunkID)
	if len(linked) != 2 {
		t.Errorf("chunk links %d entities of its own project, want 2", len(linked))
	}
}

// Not a regression: a real entity of the project that the model names
// without having been shown it in this call is still a match (§3.3c rule 1,
// the Get fallback), so today's behaviour for real ids is kept.
func runRealMatchIDOutsideCatalogCase(t *testing.T, repos GraphPipelineRepos, seed SeedChunk) {
	ctx := context.Background()
	project, chunkID := uniqueID("proj"), uniqueID("chunk")
	seedVendor(ctx, t, repos, project)
	// Not matched by NameLike "Blinds", so not in the catalog shown.
	other := &persistence.KnowledgeEntity{
		ProjectID: project, Type: persistence.EntityTypeVendor, CanonicalName: "Rolety s.r.o.",
	}
	if err := repos.Entities.Insert(ctx, other); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	if err := runMatchCase(ctx, t, repos, seed, project, chunkID, other.ID); err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	linked := assertLinksReal(ctx, t, repos, project, chunkID)
	if _, ok := linked[other.ID]; !ok {
		t.Errorf("a match to a real entity of the project must be kept")
	}
	if _, err := repos.Entities.GetByCanonical(ctx, project, persistence.EntityTypeVendor, "Blinds"); err == nil {
		t.Errorf("a kept match must not also mint an entity for the candidate")
	}
}
