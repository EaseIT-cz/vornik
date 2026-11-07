//go:build integration

package memory_test

// GitHub #618 (firewall design, "Amendment 2026-11-06 (#618)"). A role capped
// at a sensitivity tier must, on every search statement, the keyword and
// substring fallbacks included, get only chunks at or below the cap. Public is
// an operator label or derived from proof (repo-scoped, provenance-verified,
// under a configured public prefix), never from a path a writer named. Before
// the change every chunk was returned. Found at: review (the epic #619
// audit); the property is which rows the SQL returns, so the test sits at
// integration.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

type sensitivitySeed struct {
	source, status, validator, tier, class string
	scoped                                 bool
}

func seedSensitivityChunks(t *testing.T, db *sql.DB, marker string) authorityFixture {
	t.Helper()
	pipeline, _ := newIngestRecallStack(db)
	f := authorityFixture{project: persistence.GenerateID("itest-gh618"), id: map[string]string{}}
	t.Cleanup(func() { cleanupProjects(t, db, f.project) })
	prov := memory.ValidatorRoleProvenance
	seeds := map[string]sensitivitySeed{
		// Derived public: proven to be the published file.
		"published": {source: "docs/public/features/memory.md", status: "verified", validator: prov, scoped: true},
		// The path is only a claim: not verified.
		"claimed": {source: "docs/public/claimed.md", status: "unverified", scoped: true},
		// Verified under the prefix, but no repo scope.
		"unscoped": {source: "docs/public/unscoped.md", status: "verified", validator: prov},
		// Verified, but not under a public prefix.
		"design": {source: "https://docs.vornik.io", status: "verified", validator: prov, scoped: true},
		// Published, but the operator demoted it.
		"demoted": {source: "docs/public/withdrawn.md", status: "verified", validator: prov, tier: "confidential", scoped: true},
		// Unpublished, but the operator labelled it public.
		"labelled": {source: "notes/approved.md", status: "unverified", tier: "public", scoped: true},
		// Operator-labelled internal and restricted.
		"internal":   {source: "notes/internal.md", status: "unverified", tier: "internal", scoped: true},
		"restricted": {source: "notes/restricted.md", status: "unverified", tier: "restricted", scoped: true},
		// A published credentials chunk is never served under a cap.
		"credential": {source: "docs/public/creds.md", status: "verified", validator: prov, class: "credentials", scoped: true},
		// An unlabelled, unpublished chunk ranks as confidential.
		"unlabelled": {source: "notes/plain.md", status: "unverified", scoped: true},
	}
	for name, s := range seeds {
		content := "The " + marker + " index for the " + name + " chunk keeps one row per repository path."
		if st, _ := ingestForProject(t, db, pipeline, f.project, s.source, content); st.Admitted == 0 {
			t.Fatalf("%s not admitted: %+v", s.source, st)
		}
		id := chunkIDBySource(t, db, f.project, s.source)
		var scope, tier, validator, class any
		if s.scoped {
			scope = authorityScope
		}
		if s.tier != "" {
			tier = s.tier
		}
		if s.validator != "" {
			validator = s.validator
		}
		if s.class != "" {
			class = s.class
		}
		if _, err := db.Exec(`UPDATE project_memory_chunks
			SET repo_scope = $1, validation_status = $2, producer_role = 'rag-ingester', validator_role = $3,
			    sensitivity_tier = $4, content_class = COALESCE($5, content_class)
			WHERE id = $6`, scope, s.status, validator, tier, class, id); err != nil {
			t.Fatal(err)
		}
		f.id[name] = id
	}
	return f
}

func TestIntegration618_EverySearchStatementHonoursTheSensitivityCap(t *testing.T) {
	db := openIngestRecallDB(t)
	repo := memory.NewRepository(db)
	ctx := context.Background()
	const marker = "vorthexil"
	f := seedSensitivityChunks(t, db, marker)
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
	public := []string{"docs/public/"}
	cases := []struct {
		name string
		sf   memory.ScopeFilter
		want []string
	}{
		{"public cap: proven published and operator-labelled public only",
			memory.ScopeFilter{MaxSensitivity: memory.SensitivityCapPublic, PublicPrefixes: public},
			f.ids("published", "labelled")},
		{"public cap with no public prefixes: operator labels only",
			memory.ScopeFilter{MaxSensitivity: memory.SensitivityCapPublic},
			f.ids("labelled")},
		{"internal cap adds operator-labelled internal",
			memory.ScopeFilter{MaxSensitivity: memory.SensitivityCapInternal, PublicPrefixes: public},
			f.ids("published", "labelled", "internal")},
		{"confidential cap drops only restricted and credentials",
			memory.ScopeFilter{MaxSensitivity: memory.SensitivityCapConfidential, PublicPrefixes: public},
			f.ids("published", "claimed", "unscoped", "design", "demoted", "labelled", "internal", "unlabelled")},
		{"no cap: every chunk",
			memory.ScopeFilter{PublicPrefixes: public},
			f.ids("published", "claimed", "unscoped", "design", "demoted", "labelled", "internal", "restricted", "credential", "unlabelled")},
		{"an unknown cap admits nothing",
			memory.ScopeFilter{MaxSensitivity: "secret", PublicPrefixes: public}, nil},
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

// #618 C4: the doctor's denominator counts the project's chunks by tier and
// what the role's whole filter admits, with the same fragment the searches
// use. The credentials chunk is derived public (it is a published file) but
// the cap still excludes it.
func TestIntegration618_SensitivityCapCounts(t *testing.T) {
	db := openIngestRecallDB(t)
	repo := memory.NewRepository(db)
	f := seedSensitivityChunks(t, db, "quellomar")
	got, err := repo.SensitivityCapCounts(context.Background(), f.project, memory.ScopeFilter{
		RepoScope: authorityScope, MaxSensitivity: memory.SensitivityCapPublic, PublicPrefixes: []string{"docs/public/"}})
	if err != nil {
		t.Fatal(err)
	}
	want := memory.SensitivityCapCounts{Public: 1, Internal: 1, Confidential: 1, Restricted: 1, DerivedPublic: 2, Unlabelled: 4, Admitted: 2}
	if got != want {
		t.Fatalf("counts %+v, want %+v", got, want)
	}
}
