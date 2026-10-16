//go:build integration

package memory_test

// GitHub #368 (found at: review, the #142 census, wave 8 lane N, 2026-10-14;
// the level a user observes it: the companion recall over a real database).
// Design 22, "Every search path honours repo_scope and strict_scope".
//
// Every search statement a recall can reach is called on the real pgvector
// database with four chunks that share one marker and differ only in scope
// (a, b, '*', NULL). A scoped statement must return exactly the scope's set
// (lenient: a, '*', NULL; strict: a, '*'), and every hit must carry the
// scope it was stored under: design_record counts designs by it. Before the
// fix the substring tiers (S3, S6, S9) and the unscoped legacy statements
// (S7, S8) returned every hit with an empty repo_scope.
//
// The widen tests pin that a widen round never relaxes strict_scope: before
// the fix both RecallWithRouting (a strict recall with no in-scope hit is a
// low verdict, so it widens) and RecallSufficient set StrictScope=false on
// rounds 2+ and returned the NULL-scoped chunk the caller had excluded.

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

const (
	scopeA = "github.com/example/repo-a"
	scopeB = "github.com/example/repo-b"
)

// scopedFixture is one project holding a chunk per scope.
type scopedFixture struct {
	project string
	byScope map[string]string // stored scope ("" = NULL) -> chunk id
	scopeOf map[string]string // chunk id -> stored scope
}

func seedScopedChunks(t *testing.T, db *sql.DB, marker string, extra map[string]string) scopedFixture {
	t.Helper()
	pipeline, _ := newIngestRecallStack(db)
	project := persistence.GenerateID("itest-gh368")
	t.Cleanup(func() { cleanupProjects(t, db, project) })
	notes := map[string]string{
		scopeA: "The " + marker + " relay for repository alpha listens on port 7101 across every regional node.",
		scopeB: "The " + marker + " relay for repository bravo listens on port 7102 across every regional node.",
		"*":    "The " + marker + " relay convention shared by all repositories uses port 7103 on every node.",
		"":     "The " + marker + " relay legacy note without any scope says port 7104 on every regional node.",
	}
	for scope, content := range extra {
		notes[scope] = content
	}
	f := scopedFixture{project: project, byScope: map[string]string{}, scopeOf: map[string]string{}}
	for scope, content := range notes {
		source := "scope-" + strings.NewReplacer("/", "_", "*", "star").Replace(scope) + ".md"
		if st, _ := ingestForProject(t, db, pipeline, project, source, content); st.Admitted == 0 {
			t.Fatalf("%s not admitted: %+v", source, st)
		}
		id := chunkIDBySource(t, db, project, source)
		var v any
		if scope != "" {
			v = scope
		}
		if _, err := db.Exec(`UPDATE project_memory_chunks SET repo_scope = $1 WHERE id = $2`, v, id); err != nil {
			t.Fatal(err)
		}
		f.byScope[scope] = id
		f.scopeOf[id] = scope
	}
	return f
}

// embedAll gives every chunk of the project the same unit vector so the
// hybrid statements' semantic arm ranks all of them.
func embedAll(t *testing.T, db *sql.DB, repo *memory.Repository, f scopedFixture) []float32 {
	t.Helper()
	vec := make([]float32, 1024)
	vec[0] = 1
	for id := range f.scopeOf {
		if err := repo.UpdateEmbedding(context.Background(), id, vec, ""); err != nil {
			t.Fatalf("embed %s: %v", id, err)
		}
	}
	return vec
}

func (f scopedFixture) want(scopes ...string) []string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, f.byScope[s])
	}
	sort.Strings(out)
	return out
}

// assertScopedSet checks the exact id set and that every hit carries the
// scope it was stored under.
func assertScopedSet(t *testing.T, f scopedFixture, res []memory.SearchResult, want []string) {
	t.Helper()
	got := make([]string, 0, len(res))
	for _, r := range res {
		got = append(got, r.ChunkID)
		stored, ours := f.scopeOf[r.ChunkID]
		if !ours {
			t.Errorf("hit %s is not one of this project's chunks", r.ChunkID)
			continue
		}
		if r.RepoScope != stored {
			t.Errorf("hit %s carries repo_scope %q, stored %q", r.ChunkID, r.RepoScope, stored)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v (by scope: %v)", got, want, f.byScope)
	}
}

func TestIntegration_EverySearchStatementHonoursScope(t *testing.T) {
	db := openIngestRecallDB(t)
	repo := memory.NewRepository(db)
	ctx := context.Background()
	const marker = "zorbquill"
	f := seedScopedChunks(t, db, marker, nil)
	vec := embedAll(t, db, repo, f)
	var none time.Time

	lenient := f.want(scopeA, "*", "")
	strict := f.want(scopeA, "*")
	all := f.want(scopeA, scopeB, "*", "")

	// A lower date bound keeps the unscoped S4 and S5 cases on their own
	// statements: with no scope AND no date, hybridSearchTemporal
	// short-circuits to S7 (review 6a36 #6). It bounds nothing here.
	since := time.Now().AddDate(0, 0, -1)

	type call func(scope string, strict bool) ([]memory.SearchResult, error)
	scoped := map[string]call{
		"S1 HybridSearchWithEpochs": func(s string, st bool) ([]memory.SearchResult, error) {
			return repo.HybridSearchWithEpochs(ctx, f.project, vec, marker, 20, nil, true, none, none, s, st)
		},
		"S2 keywordSearchWithEpochsTemporal": func(s string, st bool) ([]memory.SearchResult, error) {
			return repo.HybridSearchWithEpochs(ctx, f.project, nil, marker, 20, nil, true, none, none, s, st)
		},
		"S3 substringSearchWithEpochsTemporal": func(s string, st bool) ([]memory.SearchResult, error) {
			return memory.SubstringSearchWithEpochsScopedForTest(ctx, repo, f.project, marker, 20, s, st)
		},
		"S4 hybridSearchTemporal": func(s string, st bool) ([]memory.SearchResult, error) {
			return repo.HybridSearchWithEpochs(ctx, f.project, vec, marker, 20, nil, false, since, none, s, st)
		},
		"S5 keywordSearchTemporal": func(s string, st bool) ([]memory.SearchResult, error) {
			return repo.HybridSearchWithEpochs(ctx, f.project, nil, marker, 20, nil, false, since, none, s, st)
		},
		"S6 substringSearchTemporal": func(s string, st bool) ([]memory.SearchResult, error) {
			return memory.SubstringSearchTemporalScopedForTest(ctx, repo, f.project, marker, 20, s, st)
		},
		"S9 substringSearch": func(s string, st bool) ([]memory.SearchResult, error) {
			return memory.SubstringSearchScopedForTest(ctx, repo, f.project, marker, 20, s, st)
		},
	}
	for name, c := range scoped {
		t.Run(name+"/lenient", func(t *testing.T) {
			res, err := c(scopeA, false)
			if err != nil {
				t.Fatal(err)
			}
			assertScopedSet(t, f, res, lenient)
		})
		t.Run(name+"/strict", func(t *testing.T) {
			res, err := c(scopeA, true)
			if err != nil {
				t.Fatal(err)
			}
			assertScopedSet(t, f, res, strict)
		})
		t.Run(name+"/unscoped", func(t *testing.T) {
			res, err := c("", false)
			if err != nil {
				t.Fatal(err)
			}
			assertScopedSet(t, f, res, all)
		})
	}

	unscoped := map[string]func() ([]memory.SearchResult, error){
		"S7 HybridSearch":  func() ([]memory.SearchResult, error) { return repo.HybridSearch(ctx, f.project, vec, marker, 20) },
		"S8 KeywordSearch": func() ([]memory.SearchResult, error) { return repo.KeywordSearch(ctx, f.project, marker, 20) },
	}
	for name, c := range unscoped {
		t.Run(name, func(t *testing.T) {
			res, err := c()
			if err != nil {
				t.Fatal(err)
			}
			assertScopedSet(t, f, res, all)
		})
	}
}

// A strict recall that finds nothing in scope is a low verdict, so routing
// widens; the widen must keep strict_scope.
func TestIntegration_RoutingWidenKeepsStrictScope(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	const marker = "zorbquill"
	const legacyOnly = "kelpwarden"
	f := seedScopedChunks(t, db, marker, map[string]string{
		"": "The " + marker + " relay legacy note mentions " + legacyOnly + " and port 7104 on every node.",
	})
	_, searcher := newIngestRecallStack(db) // routing on by default
	reqCtx := memoryfirewall.RequestContext{Purpose: memoryfirewall.PurposeOperational}

	res, verdict, err := searcher.RecallWithRouting(ctx, f.project, legacyOnly,
		memory.SearchOptions{Limit: 10, RepoScope: scopeA, StrictScope: true, Routing: true}, reqCtx)
	if err != nil {
		t.Fatal(err)
	}
	if verdict == nil || verdict.WidenRounds == 0 {
		t.Fatalf("the widen did not run (verdict %+v); the test does not exercise the widen", verdict)
	}
	assertScopedSet(t, f, res, nil)

	// The lenient recall still reaches it: the chunk is there to find.
	res, _, err = searcher.RecallWithRouting(ctx, f.project, legacyOnly,
		memory.SearchOptions{Limit: 10, RepoScope: scopeA, Routing: true}, reqCtx)
	if err != nil {
		t.Fatal(err)
	}
	assertScopedSet(t, f, res, f.want(""))
}

// scoreLegacyHigh is a deterministic reranker: the NULL-scoped legacy chunk
// scores above the sufficiency floor, everything else below it, so a widen
// round that admits the legacy chunk is "sufficient" and wins.
type scoreLegacyHigh struct{ calls int }

func (r *scoreLegacyHigh) Rerank(_ context.Context, _ string, in []memory.SearchResult) ([]memory.SearchResult, error) {
	r.calls++
	out := make([]memory.SearchResult, len(in))
	copy(out, in)
	for i := range out {
		out[i].Score = 0.1
		if strings.Contains(out[i].Content, "legacy") {
			out[i].Score = 0.9
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

func TestIntegration_SufficiencyWidenKeepsStrictScope(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	const marker = "zorbquill"
	f := seedScopedChunks(t, db, marker, nil)
	_, searcher := newIngestRecallStack(db)
	rr := &scoreLegacyHigh{}
	searcher.SetReranker(rr)
	searcher.SetSufficiency(memory.SufficiencyConfig{Enabled: true, MinHighRel: 1, ScoreFloor: 0.5, MaxRounds: 3})
	reqCtx := memoryfirewall.RequestContext{Purpose: memoryfirewall.PurposeOperational}

	res, err := searcher.RecallSufficient(ctx, f.project, marker,
		memory.SearchOptions{Limit: 10, RepoScope: scopeA, StrictScope: true}, reqCtx)
	if err != nil {
		t.Fatal(err)
	}
	if rr.calls < 2 {
		t.Fatalf("reranker called %d time(s): no widen round ran, so the test does not exercise W1", rr.calls)
	}
	assertScopedSet(t, f, res, f.want(scopeA, "*"))
}
