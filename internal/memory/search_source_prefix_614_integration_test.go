//go:build integration

package memory_test

// GitHub #614 C3 (architect design §4.2, §4.6). The A2A architect may
// retrieve only approved briefs and public docs. With SourcePrefixes set,
// every search statement must return only chunks whose source_name begins
// with one of the prefixes, a prefix matching literally (an underscore is
// not a wildcard, "docs/public/" does not match "docs/public-beta/").
// Before the change all seven chunks were returned. Found at: review (the
// epic #619 audit); the property is which rows the SQL returns, so the test
// sits at integration.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

func seedSourcePrefixChunks(t *testing.T, db *sql.DB, marker string) authorityFixture {
	t.Helper()
	pipeline, _ := newIngestRecallStack(db)
	f := authorityFixture{project: persistence.GenerateID("itest-gh614"), id: map[string]string{}}
	t.Cleanup(func() { cleanupProjects(t, db, f.project) })
	sources := map[string]string{
		"brief":       "docs/architect/memory.md",
		"public":      "docs/public/features/memory.md",
		"design":      "https://docs.vornik.io",
		"legal":       "docs/legal/dpa.md",
		"collide":     "docs/architectX/memory.md",
		"public-beta": "docs/public-beta/memory.md",
		"underscore":  "docs/architect_x/memory.md",
	}
	for name, source := range sources {
		content := "The " + marker + " index for the " + name + " source keeps one row per repository path."
		if st, _ := ingestForProject(t, db, pipeline, f.project, source, content); st.Admitted == 0 {
			t.Fatalf("%s not admitted: %+v", source, st)
		}
		id := chunkIDBySource(t, db, f.project, source)
		if _, err := db.Exec(`UPDATE project_memory_chunks
			SET repo_scope = $1, validation_status = 'verified', producer_role = 'rag-ingester', validator_role = $2
			WHERE id = $3`, authorityScope, memory.ValidatorRoleProvenance, id); err != nil {
			t.Fatal(err)
		}
		f.id[name] = id
	}
	return f
}

func TestIntegration614_EverySearchStatementHonoursSourcePrefixes(t *testing.T) {
	db := openIngestRecallDB(t)
	repo := memory.NewRepository(db)
	ctx := context.Background()
	const marker = "zelquarith"
	f := seedSourcePrefixChunks(t, db, marker)
	vec := make([]float32, 1024)
	vec[0] = 1
	for _, id := range f.id {
		if err := repo.UpdateEmbedding(ctx, id, vec, ""); err != nil {
			t.Fatalf("embed %s: %v", id, err)
		}
	}
	var none time.Time
	since := time.Now().AddDate(0, 0, -1)
	type call func(sf memory.ScopeFilter) ([]memory.SearchResult, error)
	paths := map[string]call{
		"S1 hybrid with epochs": func(sf memory.ScopeFilter) ([]memory.SearchResult, error) {
			return repo.HybridSearchWithEpochsFiltered(ctx, f.project, vec, marker, 20, nil, true, none, none, sf)
		},
		"S2 keyword with epochs": func(sf memory.ScopeFilter) ([]memory.SearchResult, error) {
			return repo.HybridSearchWithEpochsFiltered(ctx, f.project, nil, marker, 20, nil, true, none, none, sf)
		},
		"S3 substring with epochs": func(sf memory.ScopeFilter) ([]memory.SearchResult, error) {
			return memory.SubstringSearchWithEpochsFilteredForTest(ctx, repo, f.project, marker, 20, sf)
		},
		"S4 hybrid temporal": func(sf memory.ScopeFilter) ([]memory.SearchResult, error) {
			return repo.HybridSearchWithEpochsFiltered(ctx, f.project, vec, marker, 20, nil, false, since, none, sf)
		},
		"S4 hybrid, no date bound": func(sf memory.ScopeFilter) ([]memory.SearchResult, error) {
			return repo.HybridSearchWithEpochsFiltered(ctx, f.project, vec, marker, 20, nil, false, none, none, sf)
		},
		"S5 keyword temporal": func(sf memory.ScopeFilter) ([]memory.SearchResult, error) {
			return repo.HybridSearchWithEpochsFiltered(ctx, f.project, nil, marker, 20, nil, false, since, none, sf)
		},
		"S6 substring temporal": func(sf memory.ScopeFilter) ([]memory.SearchResult, error) {
			return memory.SubstringSearchTemporalFilteredForTest(ctx, repo, f.project, marker, 20, sf)
		},
		"S9 substring": func(sf memory.ScopeFilter) ([]memory.SearchResult, error) {
			return memory.SubstringSearchFilteredForTest(ctx, repo, f.project, marker, 20, sf)
		},
	}
	allowed := []string{"docs/architect/", "docs/public/"}
	cases := []struct {
		name string
		sf   memory.ScopeFilter
		want []string
	}{
		{"the two prefixes, with the architect's authority",
			memory.ScopeFilter{RepoScope: authorityScope, Strict: true, Authority: memory.AuthorityProvenance, SourcePrefixes: allowed},
			f.ids("brief", "public")},
		{"prefixes alone narrow the search",
			memory.ScopeFilter{SourcePrefixes: allowed}, f.ids("brief", "public")},
		{"no prefixes: every source", memory.ScopeFilter{RepoScope: authorityScope},
			f.ids("brief", "public", "design", "legal", "collide", "public-beta", "underscore")},
		{"a malformed prefix fails closed",
			memory.ScopeFilter{SourcePrefixes: []string{"docs/architect/", "docs/%/"}}, nil},
	}
	for pname, c := range paths {
		for _, tc := range cases {
			t.Run(pname+"/"+tc.name, func(t *testing.T) {
				res, err := c(tc.sf)
				if err != nil {
					t.Fatal(err)
				}
				f.assertSet(t, res, tc.want)
			})
		}
	}
}
