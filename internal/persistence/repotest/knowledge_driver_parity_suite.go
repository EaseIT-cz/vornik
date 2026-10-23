package repotest

import (
	"context"
	"sort"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// runKnowledgeEntityNameLikeCases pins KnowledgeEntityFilter.NameLike to one
// rule on both drivers (GitHub #449, security audit 2026-10-17): a literal
// substring, case-insensitive for ASCII A-Z only. Before the fix SQLite ran
// LIKE and Postgres ILIKE, so a non-ASCII case difference matched on one
// driver only, and on BOTH a query of "%" or "_" listed every entity of the
// project. Called from RunKnowledgeEntitySuite, so the SQLite unit lane and
// the Postgres integration lane run the same assertions.
func runKnowledgeEntityNameLikeCases(t *testing.T, repo persistence.KnowledgeEntityRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj-namelike")
	names := []string{
		"JOHN DOE",
		"Jane Smith",
		"JÜRGEN Weiß",
		"a_b corp",
		"aXb ltd",
		"100% Beef",
		`back\slash`,
	}
	for _, n := range names {
		if err := repo.Insert(ctx, &persistence.KnowledgeEntity{
			ProjectID: project, Type: "person", CanonicalName: n,
		}); err != nil {
			t.Fatalf("seed %q: %v", n, err)
		}
	}
	list := func(t *testing.T, q string) []string {
		t.Helper()
		got, err := repo.List(ctx, persistence.KnowledgeEntityFilter{ProjectID: project, NameLike: q, Limit: 100})
		if err != nil {
			t.Fatalf("List(NameLike=%q): %v", q, err)
		}
		out := make([]string, 0, len(got))
		for _, e := range got {
			out = append(out, e.CanonicalName)
		}
		sort.Strings(out)
		return out
	}
	cases := []struct {
		name string
		q    string
		want []string
	}{
		// ASCII case folds in both directions.
		{"NameLike_ASCII_lower_query_matches_upper_name", "john", []string{"JOHN DOE"}},
		{"NameLike_ASCII_upper_query_matches_mixed_name", "JANE", []string{"Jane Smith"}},
		// A non-ASCII letter must match exactly: Postgres ILIKE under a
		// UTF-8 locale folded it, SQLite LIKE did not.
		{"NameLike_non_ASCII_case_difference_does_not_match", "jürgen", nil},
		{"NameLike_non_ASCII_exact_case_matches", "jÜrgen", []string{"JÜRGEN Weiß"}},
		{"NameLike_non_ASCII_is_not_accent_folded", "jurgen", nil},
		// Wildcards and the escape character match only themselves.
		{"NameLike_percent_is_literal", "%", []string{"100% Beef"}},
		{"NameLike_underscore_is_literal", "_", []string{"a_b corp"}},
		{"NameLike_underscore_does_not_match_any_char", "a_b", []string{"a_b corp"}},
		{"NameLike_backslash_is_literal", `\`, []string{`back\slash`}},
		{"NameLike_backslash_does_not_escape_next_char", `k\s`, []string{`back\slash`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := list(t, tc.q)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("NameLike=%q matched %q, want %q", tc.q, got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("NameLike=%q matched %q, want %q", tc.q, got, want)
				}
			}
		})
	}
}

// runDropChunkFromSourcesCases pins KnowledgeEdgeRepository.DropChunkFromSources
// on both drivers (GitHub #450, security audit 2026-10-17): the count is the
// number of edges whose source_chunks was modified, and an edge left with no
// source is quarantined. Before the fix Postgres returned only the count of the
// outer UPDATE of a data-modifying CTE (the quarantined subset). The issue's
// five-edge scenario (four edges keep another chunk, one has only c1) plus an
// edge holding [a, c1, b]; the answer is 6 on both drivers.
func runDropChunkFromSourcesCases(t *testing.T, repo persistence.KnowledgeEdgeRepository, entityRepo persistence.KnowledgeEntityRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj-dropchunk")
	// Chunk ids are global to the edge table, so they are unique per run.
	c1 := uniqueID("c1")
	other := uniqueID("cother")
	longer := c1 + "x" // a LIKE-substring false positive on the old SQLite path
	before, after := uniqueID("ca"), uniqueID("cb")
	ents := make([]*persistence.KnowledgeEntity, 8) // ents[0] is every edge's source
	for i := range ents {
		ents[i] = &persistence.KnowledgeEntity{ProjectID: project, Type: "person", CanonicalName: uniqueID("dc-ent")}
		if err := entityRepo.Insert(ctx, ents[i]); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
	}
	type edgeCase struct {
		name      string
		seed      []string
		wantAfter []string
		wantState string
	}
	cases := []edgeCase{
		{"DropChunkFromSources_removes_the_chunk_and_keeps_the_rest_1", []string{c1, other}, []string{other}, "published"},
		{"DropChunkFromSources_removes_the_chunk_and_keeps_the_rest_2", []string{c1, other}, []string{other}, "published"},
		{"DropChunkFromSources_removes_the_chunk_and_keeps_the_rest_3", []string{c1, other}, []string{other}, "published"},
		{"DropChunkFromSources_removes_the_chunk_and_keeps_the_rest_4", []string{c1, other}, []string{other}, "published"},
		{"DropChunkFromSources_quarantines_an_edge_left_without_source", []string{c1}, nil, "quarantined"},
		{"DropChunkFromSources_leaves_a_longer_id_alone", []string{longer}, []string{longer}, "published"},
		{"DropChunkFromSources_keeps_the_survivors_in_order", []string{before, c1, after}, []string{before, after}, "published"},
	}
	for i, tc := range cases {
		if err := repo.UpsertEdge(ctx, &persistence.KnowledgeEdge{
			ProjectID: project, FromEntity: ents[0].ID, ToEntity: ents[i+1].ID,
			Predicate: "works_with", SourceChunks: tc.seed, Confidence: 0.5,
		}); err != nil {
			t.Fatalf("seed edge %s: %v", tc.name, err)
		}
	}

	n, err := repo.DropChunkFromSources(ctx, c1)
	if err != nil {
		t.Fatalf("DropChunkFromSources: %v", err)
	}
	t.Run("DropChunkFromSources_counts_every_modified_edge", func(t *testing.T) {
		if n != 6 {
			t.Fatalf("DropChunkFromSources = %d, want 6 (edges whose source_chunks changed)", n)
		}
	})

	edges, err := repo.List(ctx, persistence.KnowledgeEdgeFilter{
		ProjectID: project, Lifecycle: []string{"published", "quarantined"}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byTo := map[string]*persistence.KnowledgeEdge{}
	for _, e := range edges {
		byTo[e.ToEntity] = e
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertEdgeAfterDrop(t, byTo[ents[i+1].ID], tc.wantAfter, tc.wantState)
		})
	}
	t.Run("DropChunkFromSources_again_modifies_nothing", func(t *testing.T) {
		n2, err := repo.DropChunkFromSources(ctx, c1)
		if err != nil || n2 != 0 {
			t.Fatalf("second DropChunkFromSources = %d, %v; want 0, nil", n2, err)
		}
	})
}

func assertEdgeAfterDrop(t *testing.T, e *persistence.KnowledgeEdge, wantChunks []string, wantState string) {
	t.Helper()
	if e == nil {
		t.Fatal("edge missing")
	}
	ok := len(e.SourceChunks) == len(wantChunks)
	for i := 0; ok && i < len(wantChunks); i++ {
		ok = e.SourceChunks[i] == wantChunks[i]
	}
	if !ok {
		t.Errorf("source_chunks = %v, want %v (in order)", e.SourceChunks, wantChunks)
	}
	if e.LifecycleState != wantState {
		t.Errorf("lifecycle = %s, want %s", e.LifecycleState, wantState)
	}
}

// skillRoleFilterIsExactMembership pins SkillListFilter.Role on both drivers
// (lane W16-I, the #449 sweep): Role selects skills whose roles list holds
// exactly that string (case-sensitive) or is empty. Before, both drivers ran
// roles LIKE '%"<role>"%' with the caller's role unescaped (it reaches the
// repository from the skill_search MCP tool and /skills/fetch), so a role of
// "%" or "_" matched every role-scoped skill, and SQLite folded case where
// Postgres did not.
func skillRoleFilterIsExactMembership(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	p := uniqueID("proj-skillrole")
	scope := "github.com/x/role-parity"
	mk := func(id string, roles []string) {
		s := newTestSkill(uniqueID(id), p, scope, id)
		s.Maturity = persistence.SkillMaturityActive
		s.Roles = roles
		mustCreateSkill(t, repo, s)
	}
	mk("role-researcher", []string{"researcher"})
	mk("role-writer", []string{"writer", "researcher-lead"})
	mk("role-odd", []string{`a_b`, `x%y`, `q"uote`, `back\slash`})
	mk("role-any", nil)
	names := func(role string) []string {
		got, err := repo.List(ctx, p, persistence.SkillListFilter{RepoScope: scope, Role: role})
		if err != nil {
			t.Fatalf("List(Role=%q): %v", role, err)
		}
		out := make([]string, 0, len(got))
		for _, s := range got {
			out = append(out, s.Name)
		}
		sort.Strings(out)
		return out
	}
	cases := []struct {
		role string
		want []string
	}{
		{"researcher", []string{"role-any", "role-researcher"}},
		{"Researcher", []string{"role-any"}},
		{"researcher-lead", []string{"role-any", "role-writer"}},
		{"%", []string{"role-any"}},
		{"_", []string{"role-any"}},
		{"a_b", []string{"role-any", "role-odd"}},
		{"aXb", []string{"role-any"}},
		{"x%y", []string{"role-any", "role-odd"}},
		{`q"uote`, []string{"role-any", "role-odd"}},
		{`back\slash`, []string{"role-any", "role-odd"}},
	}
	for _, tc := range cases {
		got := names(tc.role)
		if len(got) != len(tc.want) {
			t.Errorf("Role=%q matched %q, want %q", tc.role, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("Role=%q matched %q, want %q", tc.role, got, tc.want)
				break
			}
		}
	}
}
