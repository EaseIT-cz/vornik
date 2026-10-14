package graph

// LAW: every knowledge-graph read that serves a person or a model honours the
// GDPR Art 18 restriction.
//
// audit 2026-10-12 (GitHub #350): an Art 18 restriction withheld the subject's
// memory chunks, but the subject's linked knowledge-graph entity still answered
// FindEntities / GetEntity (UI KG pages, the memory_search graph block that is
// put in a model's prompt), its edges were listed, and Subgraph walked through
// it to its neighbours. This file pins the rule two ways and publishes what it
// examined each run:
//
//  1. behavioural — every method of KnowledgeGraphSearcher has a probe, and a
//     new method fails here until it has one;
//  2. static — every call of an entity/edge/mention read outside the drivers is
//     in a SCREENED function or on the exemption list with its reason.
//
// see LLD § https://docs.vornik.io §4.16.7

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// fakeRestrictionScreen answers from fixed sets, or fails.
type fakeRestrictionScreen struct {
	entities map[string]bool
	chunks   map[string]bool
	err      error
}

func pick(set map[string]bool, ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		if set[id] {
			out[id] = true
		}
	}
	return out
}

func (f *fakeRestrictionScreen) RestrictedEntities(_ context.Context, ids []string) (map[string]bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	return pick(f.entities, ids), nil
}

func (f *fakeRestrictionScreen) RestrictedChunks(_ context.Context, ids []string) (map[string]bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	return pick(f.chunks, ids), nil
}

// restrictedFixture: jane is restricted. acme is connected to jane (e1), to beta
// by an edge with unrestricted evidence (e2), and to delta by an edge whose ONLY
// evidence is a restricted chunk (e4). gamma is reachable only through jane (e3).
// Chunk cR is restricted; c1 is not; the chunk lookup is deliberately one that
// does NOT filter cR, so the Searcher's own screen is what is tested.
//
// Ids that must never appear in any output: jane, e1, e3, e4, cR, cJ. gamma
// (reachable only via jane) and delta (only via e4) must not be REACHED by a
// traversal, though a name search still finds them: they are not restricted.
func restrictedFixture(screen persistence.RestrictionScreen) *Searcher {
	const p = "p"
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{
		"jane":  ent("jane", p, "PERSON", "Jane Doe"),
		"acme":  ent("acme", p, "ORG", "Acme"),
		"beta":  ent("beta", p, "ORG", "Beta"),
		"gamma": ent("gamma", p, "PERSON", "Gamma"),
		"delta": ent("delta", p, "ORG", "Delta"),
	}}
	e1 := edge("e1", p, "jane", "acme", "WORKS_AT")
	e2 := edge("e2", p, "acme", "beta", "PARTNERS_WITH")
	e2.SourceChunks = []string{"c1", "cR"} // mixed evidence: stays
	e3 := edge("e3", p, "jane", "gamma", "KNOWS")
	e4 := edge("e4", p, "acme", "delta", "SUPPLIES")
	e4.SourceChunks = []string{"cR"} // only restricted evidence: withheld
	edges := &searcherEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{
		"jane":  {e1, e3},
		"acme":  {e1, e2, e4},
		"beta":  {e2},
		"gamma": {e3},
		"delta": {e4},
	}}
	mentions := &searcherMentionRepo{byEntity: map[string][]*persistence.EntityMention{
		"jane": {{ChunkID: "cJ", EntityID: "jane", Surface: "Jane"}},
		"acme": {{ChunkID: "cR", EntityID: "acme", Surface: "Acme"}, {ChunkID: "c1", EntityID: "acme", Surface: "Acme"}},
	}}
	lookup := &fakeChunkLookup{chunks: map[string]MentionedChunk{
		"cJ": {ChunkID: "cJ", ProjectID: p, Content: "about Jane"},
		"cR": {ChunkID: "cR", ProjectID: p, Content: "restricted"},
		"c1": {ChunkID: "c1", ProjectID: p, Content: "free"},
	}}
	s := NewSearcher(er, edges, mentions, lookup, nil)
	if screen != nil {
		s.SetRestrictionScreen(screen)
	}
	return s
}

func janeRestricted() *fakeRestrictionScreen {
	return &fakeRestrictionScreen{
		entities: map[string]bool{"jane": true},
		chunks:   map[string]bool{"cR": true, "cJ": true},
	}
}

var neverVisible = []string{"jane", "e1", "e3", "e4", "cR", "cJ"}

// notReachedByTraversal are unrestricted entities reachable only through
// withheld rows: a traversal (edges, subgraph) must not arrive at them.
var notReachedByTraversal = []string{"gamma", "delta"}

// probe runs one Searcher method over the fixture and returns every id it
// surfaced (entities, edges, chunks), the ids it must still surface, and its error.
type probe func(ctx context.Context, s *Searcher) (got []string, mustSee []string, err error)

// traversals are the probes whose output follows edges.
var traversals = map[string]bool{"GetEntity": true, "Subgraph": true}

func entityIDs(es []*persistence.KnowledgeEntity) []string {
	var out []string
	for _, e := range es {
		if e != nil {
			out = append(out, e.ID)
		}
	}
	return out
}

func edgeIDs(es []*persistence.KnowledgeEdge) []string {
	var out []string
	for _, e := range es {
		if e != nil {
			out = append(out, e.ID, e.FromEntity, e.ToEntity)
		}
	}
	return out
}

var graphReadProbes = map[string]probe{
	"FindEntities": func(ctx context.Context, s *Searcher) ([]string, []string, error) {
		es, err := s.FindEntities(ctx, "p", "", nil, 50)
		return entityIDs(es), []string{"acme", "beta", "gamma", "delta"}, err
	},
	"GetEntity": func(ctx context.Context, s *Searcher) ([]string, []string, error) {
		var got []string
		e, out, in, err := s.GetEntity(ctx, "p", "jane")
		if err != nil {
			return nil, nil, err
		}
		if e != nil {
			got = append(got, e.ID)
		}
		got = append(got, edgeIDs(out)...)
		got = append(got, edgeIDs(in)...)
		e, out, in, err = s.GetEntity(ctx, "p", "acme")
		if e != nil {
			got = append(got, e.ID)
		}
		got = append(got, edgeIDs(out)...)
		got = append(got, edgeIDs(in)...)
		return got, []string{"acme", "e2", "beta"}, err
	},
	"ChunksMentioning": func(ctx context.Context, s *Searcher) ([]string, []string, error) {
		var got []string
		cs, err := s.ChunksMentioning(ctx, "p", "jane", "", 50)
		if err != nil {
			return nil, nil, err
		}
		for _, c := range cs {
			got = append(got, c.ChunkID)
		}
		cs, err = s.ChunksMentioning(ctx, "p", "acme", "", 50)
		for _, c := range cs {
			got = append(got, c.ChunkID)
		}
		return got, []string{"c1"}, err
	},
	"Subgraph": func(ctx context.Context, s *Searcher) ([]string, []string, error) {
		var got []string
		sg, err := s.Subgraph(ctx, "p", []string{"jane"}, 3)
		if err != nil {
			return nil, nil, err
		}
		got = append(got, entityIDs(sg.Entities)...)
		got = append(got, edgeIDs(sg.Edges)...)
		sg, err = s.Subgraph(ctx, "p", []string{"acme"}, 3)
		if sg != nil {
			got = append(got, entityIDs(sg.Entities)...)
			got = append(got, edgeIDs(sg.Edges)...)
		}
		return got, []string{"acme", "beta", "e2"}, err
	},
}

func TestLaw_GraphReadsHonourTheRestriction_EveryMethodProbed(t *testing.T) {
	ctx := context.Background()
	it := reflect.TypeOf((*KnowledgeGraphSearcher)(nil)).Elem()
	probed := 0
	for i := 0; i < it.NumMethod(); i++ {
		name := it.Method(i).Name
		p, ok := graphReadProbes[name]
		if !ok {
			t.Errorf("KnowledgeGraphSearcher.%s has no restriction probe: add one to graphReadProbes", name)
			continue
		}
		probed++
		t.Run(name, func(t *testing.T) {
			got, mustSee, err := p(ctx, restrictedFixture(janeRestricted()))
			if err != nil {
				t.Fatalf("restricted read: %v", err)
			}
			seen := map[string]bool{}
			for _, id := range got {
				seen[id] = true
			}
			forbidden := neverVisible
			if traversals[name] {
				forbidden = append(append([]string{}, neverVisible...), notReachedByTraversal...)
			}
			for _, id := range forbidden {
				if seen[id] {
					t.Errorf("%s surfaced %q, which the restriction withholds (got %v)", name, id, got)
				}
			}
			for _, id := range mustSee {
				if !seen[id] {
					t.Errorf("%s no longer surfaces unrestricted %q (got %v)", name, id, got)
				}
			}

			// Default-deny: no screen wired means no read at all.
			if got, _, err := p(ctx, restrictedFixture(nil)); err == nil || !errors.Is(err, ErrRestrictionScreenMissing) {
				t.Errorf("%s without a restriction screen: err=%v got=%v; want ErrRestrictionScreenMissing", name, err, got)
			}

			// Fail closed: a screen that errors yields an error and no rows.
			boom := &fakeRestrictionScreen{err: errors.New("db down")}
			if got, _, err := p(ctx, restrictedFixture(boom)); err == nil || len(got) != 0 {
				t.Errorf("%s with a failing screen: err=%v got=%v; want an error and nothing", name, err, got)
			}
		})
	}
	t.Logf("restriction law (behavioural): %d KnowledgeGraphSearcher methods, %d probed", it.NumMethod(), probed)
}

// A lift is effective on the very next read: no cache sits between the screen
// and the caller.
func TestSearcher_RestrictionLiftIsImmediate(t *testing.T) {
	ctx := context.Background()
	screen := janeRestricted()
	s := restrictedFixture(screen)
	if e, _, _, err := s.GetEntity(ctx, "p", "jane"); err != nil || e != nil {
		t.Fatalf("restricted: e=%v err=%v", e, err)
	}
	screen.entities = map[string]bool{}
	if e, _, _, err := s.GetEntity(ctx, "p", "jane"); err != nil || e == nil {
		t.Fatalf("after lift: e=%v err=%v", e, err)
	}
}

// --- static half -----------------------------------------------------------

// graphReadMethods are the read methods of the entity / edge / mention repositories.
var graphReadMethods = map[string]bool{
	"Get": true, "GetByCanonical": true, "List": true, "SimilarByEmbedding": true,
	"EdgesForEntity": true, "ListByEntity": true, "ListByChunk": true,
}

// graphReadReceivers are the receiver names those repositories are held under.
var graphReadReceivers = map[string]bool{
	"entities": true, "edges": true, "mentions": true,
	"knowledgeentities": true, "knowledgeedges": true, "entitymentions": true,
}

// graphReadScreened are functions whose every result reaches the caller only
// through the restriction screen (probed above and in query_expander_test.go).
// findEntitiesUnscreened is listed because its ONLY caller, FindEntities,
// screens what it returns.
var graphReadScreened = map[string]bool{
	"internal/memory/graph/searcher.go:findEntitiesUnscreened": true,
	"internal/memory/graph/searcher.go:GetEntity":              true,
	"internal/memory/graph/searcher.go:ChunksMentioning":       true,
	"internal/memory/graph/searcher.go:Subgraph":               true,
	"internal/memory/query_expander.go:Expand":                 true,
}

// graphReadExempt are deliberate unscreened reads, with the reason.
var graphReadExempt = map[string]string{
	"internal/memory/graph/resolver.go:shortlist": "extraction WRITE path: hiding a restricted entity from the resolver " +
		"would mint a second, unlinked entity for the same person (design §4.16.7 residual + open question)",
	"internal/memory/graph/pipeline.go:insertEntityIdempotent": "extraction WRITE path: the unique-key lookup must find " +
		"the existing (restricted) entity or the insert duplicates it",
}

func graphRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func TestLaw_GraphReadsHonourTheRestriction_EveryCallSiteClassified(t *testing.T) {
	root := graphRepoRoot(t)
	sites := 0
	funcs := map[string]bool{}
	var unclassified []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "internal/persistence/postgres/") || strings.HasPrefix(rel, "internal/persistence/sqlite/") {
				return nil // the drivers implement the reads; this law is about their callers
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				key := rel + ":" + fd.Name.Name
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !graphReadMethods[sel.Sel.Name] {
						return true
					}
					var recv string
					switch x := sel.X.(type) {
					case *ast.SelectorExpr:
						recv = x.Sel.Name
					case *ast.Ident:
						recv = x.Name
					}
					if !graphReadReceivers[strings.ToLower(recv)] {
						return true
					}
					sites++
					if !funcs[key] {
						funcs[key] = true
						if !graphReadScreened[key] && graphReadExempt[key] == "" {
							unclassified = append(unclassified, key)
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(unclassified)
	for _, k := range unclassified {
		t.Errorf("%s reads knowledge-graph rows without the Art 18 restriction screen: screen it, or add it to graphReadExempt with the reason", k)
	}
	screened, exempt := 0, 0
	for k := range funcs {
		if graphReadScreened[k] {
			screened++
		} else if graphReadExempt[k] != "" {
			exempt++
		}
	}
	for k := range graphReadScreened {
		if !funcs[k] {
			t.Errorf("graphReadScreened names %s, which no longer reads graph rows: remove it", k)
		}
	}
	for k := range graphReadExempt {
		if !funcs[k] {
			t.Errorf("graphReadExempt names %s, which no longer reads graph rows: remove it", k)
		}
	}
	t.Logf("restriction law (static): %d graph read call sites in %d functions — %d screened, %d exempt, %d unclassified",
		sites, len(funcs), screened, exempt, len(unclassified))

	// The denominator is ASSERTED, not only printed (design review
	// task_20261012224435_a3ccf77ae7da0a21 F1): a renamed receiver drops a call
	// site out of the sweep silently, and only a pinned count turns that into a
	// red. A change here is a reviewed change: update the pin AND design §4.16.7.
	if sites != pinnedGraphReadSites || len(funcs) != pinnedGraphReadFuncs {
		t.Errorf("graph read denominator moved: %d call sites in %d functions, pinned %d in %d — "+
			"classify the change, then update the pin and design §4.16.7", sites, len(funcs),
			pinnedGraphReadSites, pinnedGraphReadFuncs)
	}
}

const (
	pinnedGraphReadSites = 15
	pinnedGraphReadFuncs = 7
)

// The sweep's method list is closed, so a NEW read method on a graph repository
// would be invisible to it (review F1). Every method of the three repository
// interfaces is either a write (by its verb) or in graphReadMethods.
func TestLaw_GraphReadsHonourTheRestriction_EveryRepositoryReadMethodSwept(t *testing.T) {
	writeVerbs := []string{"Insert", "Update", "Upsert", "Add", "Drop", "Delete"}
	n := 0
	for _, it := range []reflect.Type{
		reflect.TypeOf((*persistence.KnowledgeEntityRepository)(nil)).Elem(),
		reflect.TypeOf((*persistence.KnowledgeEdgeRepository)(nil)).Elem(),
		reflect.TypeOf((*persistence.EntityMentionRepository)(nil)).Elem(),
	} {
		for i := 0; i < it.NumMethod(); i++ {
			name := it.Method(i).Name
			n++
			isWrite := false
			for _, v := range writeVerbs {
				if strings.HasPrefix(name, v) {
					isWrite = true
				}
			}
			if !isWrite && !graphReadMethods[name] {
				t.Errorf("%s.%s reads graph rows but is not in graphReadMethods: the call-site sweep cannot see it", it.Name(), name)
			}
		}
	}
	t.Logf("restriction law (repository surface): %d methods examined", n)
}
