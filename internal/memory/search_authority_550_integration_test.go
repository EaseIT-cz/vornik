//go:build integration

package memory_test

// GitHub #550 (found at: production, the operator's question of 2026-10-25,
// compared as e2e-lane; this test sits lower, at integration, because the
// property is which rows the search SQL returns, and the higher levels add
// only transport). Design: https://docs.vornik.io
// §2.1 C1 and §2.5.
//
// The Vornik architect answers customer A2A calls from memory_search. Before
// this change every search statement returned a self-stamped review, a
// companion note, a provenance:unavailable chunk and an operator correction
// beside the provenance-verified design, ranked on relevance alone. With
// Authority "provenance" each statement must return only chunks the daemon
// checked against the trusted branch.

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

const authorityScope = "github.com/example/vornik"

// authorityFixture holds one project with a chunk per stamp shape.
type authorityFixture struct {
	project string
	id      map[string]string // shape -> chunk id
}

func seedAuthorityChunks(t *testing.T, db *sql.DB, marker string) authorityFixture {
	t.Helper()
	pipeline, _ := newIngestRecallStack(db)
	f := authorityFixture{project: persistence.GenerateID("itest-gh550"), id: map[string]string{}}
	t.Cleanup(func() {
		cleanupProjects(t, db, f.project)
		_, _ = db.Exec(`DELETE FROM artifacts WHERE project_id = $1`, f.project)
	})
	type shape struct {
		scope, status, producer, validator, artifact string
	}
	shapes := map[string]shape{
		"design":      {authorityScope, "verified", "rag-ingester", memory.ValidatorRoleProvenance, "a-design"},
		"review":      {authorityScope, "verified", "reviewer", "reviewer", ""},
		"note":        {authorityScope, "verified", "companion:claude-code", "companion:claude-code", ""},
		"unavailable": {authorityScope, "verified", "rag-ingester", memory.ValidatorRoleProvenanceUnavailable, "a-unavail"},
		"correction":  {authorityScope, "verified", "operator_correction", "", ""},
		"other-repo":  {"github.com/example/other", "verified", "rag-ingester", memory.ValidatorRoleProvenance, "a-other"},
		"null-scope":  {"", "verified", "rag-ingester", memory.ValidatorRoleProvenance, "a-null"},
	}
	for name, s := range shapes {
		source := "authority-" + name + ".md"
		content := "The " + marker + " gateway for the " + name + " shape listens on its own port across every regional node."
		if st, _ := ingestForProject(t, db, pipeline, f.project, source, content); st.Admitted == 0 {
			t.Fatalf("%s not admitted: %+v", source, st)
		}
		id := chunkIDBySource(t, db, f.project, source)
		var scope, validator, artifact any
		if s.scope != "" {
			scope = s.scope
		}
		if s.validator != "" {
			validator = s.validator
		}
		if s.artifact != "" {
			aid := f.project + "-" + s.artifact
			if _, err := db.Exec(`INSERT INTO artifacts (id, project_id, name, artifact_class, storage_path, origin, created_at)
				VALUES ($1, $2, 'x.md', 'INPUT'::artifact_class, '/dev/null', 'upload', NOW())`, aid, f.project); err != nil {
				t.Fatalf("insert artifact: %v", err)
			}
			artifact = aid
		}
		if _, err := db.Exec(`UPDATE project_memory_chunks
			SET repo_scope = $1, validation_status = $2, producer_role = $3, validator_role = $4, artifact_id = $5
			WHERE id = $6`, scope, s.status, s.producer, validator, artifact, id); err != nil {
			t.Fatal(err)
		}
		f.id[name] = id
	}
	return f
}

func (f authorityFixture) ids(names ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, f.id[n])
	}
	sort.Strings(out)
	return out
}

func (f authorityFixture) assertSet(t *testing.T, res []memory.SearchResult, want []string) {
	t.Helper()
	byID := map[string]string{}
	for n, id := range f.id {
		byID[id] = n
	}
	got := make([]string, 0, len(res))
	for _, r := range res {
		got = append(got, r.ChunkID)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		names := make([]string, 0, len(got))
		for _, id := range got {
			names = append(names, byID[id])
		}
		t.Errorf("returned %v, want only %v", names, want)
	}
}

func TestIntegration550_EverySearchStatementHonoursAuthority(t *testing.T) {
	db := openIngestRecallDB(t)
	repo := memory.NewRepository(db)
	ctx := context.Background()
	const marker = "quorvantile"
	f := seedAuthorityChunks(t, db, marker)
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
		"S4 hybrid, no date bound (must not take the unscoped shortcut)": func(sf memory.ScopeFilter) ([]memory.SearchResult, error) {
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
	cases := []struct {
		name string
		sf   memory.ScopeFilter
		want []string
	}{
		{"scoped, authority provenance", memory.ScopeFilter{RepoScope: authorityScope, Authority: memory.AuthorityProvenance}, f.ids("design")},
		{"unscoped, authority provenance", memory.ScopeFilter{Authority: memory.AuthorityProvenance}, f.ids("design", "other-repo")},
		// A provenance-verified chunk whose scope was retagged to NULL names no
		// repository, so it is not authoritative under a lenient scope either.
		{"scoped lenient, authority provenance, NULL scope excluded", memory.ScopeFilter{RepoScope: authorityScope, Strict: false, Authority: memory.AuthorityProvenance}, f.ids("design")},
		{"scoped strict, authority any", memory.ScopeFilter{RepoScope: authorityScope, Strict: true}, f.ids("design", "review", "note", "unavailable", "correction")},
		{"unknown authority fails closed", memory.ScopeFilter{RepoScope: authorityScope, Authority: "role-of-record"}, nil},
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
