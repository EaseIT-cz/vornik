package repotest

import (
	"context"
	"sort"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Shared suites for the knowledge-graph read path under GDPR Art 18 (GitHub
// #349, #350; design 2026-07-29-gdpr-data-subject-rights-design.md §4.16.7).

// RestrictedEntityID is the id of the entity a RestrictChunk(viaEntity=true)
// helper creates and links for chunkID. Both drivers' helpers follow it.
func RestrictedEntityID(chunkID string) string { return "ent_" + chunkID }

// SetChunkState sets a seeded chunk's lifecycle, validation status and repo
// scope ("" scope = NULL).
type SetChunkState func(ctx context.Context, chunkID, lifecycle, validation, repoScope string) error

// GraphChunkLookup is graph.SQLChunkLookup.LookupChunks reduced to the ids it
// returns (repotest cannot import the graph package's types without coupling
// the suite to it; the caller adapts).
type GraphChunkLookup func(ctx context.Context, projectID string, chunkIDs []string, repoScope string) ([]string, error)

// RunGraphChunkLookupSuite pins the graph chunk lookup identically on both
// drivers: project scope, repo-scope (migration-75) semantics, the published
// lifecycle, the refuted/superseded exclusion, and the Art 18 restriction by
// both link routes plus its lift.
//
// audit 2026-10-12 (GitHub #349): on SQLite this lookup errored on every call —
// its query names lifecycle_state, which the SQLite chunk table lacked.
func RunGraphChunkLookupSuite(t *testing.T, lookup GraphChunkLookup, seed SeedChunk, set SetChunkState, restrict RestrictChunk) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	ids, foreign := seedGraphLookupChunks(ctx, t, project, seed, set)
	liftDirect, err := restrict(ctx, ids["direct"], project, false)
	if err != nil {
		t.Fatalf("restrict direct: %v", err)
	}
	liftEnt, err := restrict(ctx, ids["viaEntity"], project, true)
	if err != nil {
		t.Fatalf("restrict via entity: %v", err)
	}

	all := []string{foreign}
	for _, id := range ids {
		all = append(all, id)
	}
	names := func(scope string) []string {
		t.Helper()
		got, err := lookup(ctx, project, all, scope)
		if err != nil {
			t.Fatalf("LookupChunks(scope=%q): %v", scope, err)
		}
		byID := map[string]string{foreign: "foreign"}
		for n, id := range ids {
			byID[id] = n
		}
		var out []string
		for _, id := range got {
			out = append(out, byID[id])
		}
		sort.Strings(out)
		return out
	}
	assertNameSet(t, "project-wide", names(""), []string{"nullScope", "scopeA", "scopeB", "star"})
	assertNameSet(t, "scope repoA", names("repoA"), []string{"nullScope", "scopeA", "star"})

	if err := liftDirect(); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if err := liftEnt(); err != nil {
		t.Fatalf("lift: %v", err)
	}
	assertNameSet(t, "after the lift", names(""), []string{"direct", "nullScope", "scopeA", "scopeB", "star", "viaEntity"})
}

func assertNameSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if len(got) != len(want) {
		t.Errorf("%s: got %v, want %v", label, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: got %v, want %v", label, got, want)
			return
		}
	}
}

// seedGraphLookupChunks seeds one chunk per visibility case in project (named
// by the returned map) and one chunk in a foreign project.
func seedGraphLookupChunks(ctx context.Context, t *testing.T, project string, seed SeedChunk, set SetChunkState) (map[string]string, string) {
	t.Helper()
	foreignProject := uniqueID("proj")
	ids := map[string]string{}
	for _, name := range []string{"scopeA", "scopeB", "star", "nullScope", "quarantined", "refuted", "direct", "viaEntity"} {
		ids[name] = uniqueID("chunk")
	}
	foreign := uniqueID("chunk")
	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	i := 0
	for _, id := range ids {
		if err := seed(ctx, id, project, "content of "+id, false, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		i++
	}
	if err := seed(ctx, foreign, foreignProject, "foreign", false, base); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}
	states := map[string][3]string{
		"scopeA":      {"published", "unverified", "repoA"},
		"scopeB":      {"published", "verified", "repoB"},
		"star":        {"published", "unverified", "*"},
		"nullScope":   {"published", "unverified", ""},
		"quarantined": {"quarantined", "unverified", ""},
		"refuted":     {"published", "refuted", ""},
		"direct":      {"published", "unverified", ""},
		"viaEntity":   {"published", "unverified", ""},
	}
	for name, st := range states {
		if err := set(ctx, ids[name], st[0], st[1], st[2]); err != nil {
			t.Fatalf("set state %s: %v", name, err)
		}
	}
	return ids, foreign
}

// RunRestrictionScreenSuite pins persistence.SQLRestrictionScreen on both
// drivers: a directly linked chunk, a chunk mentioning a linked entity, and the
// linked entity itself are restricted; nothing else is; a lift clears all three.
//
// audit 2026-10-12 (GitHub #350): the knowledge-graph Searcher reads through
// this screen, so its SQL must answer identically on SQLite and Postgres.
func RunRestrictionScreenSuite(t *testing.T, screen persistence.RestrictionScreen, seed SeedChunk, restrict RestrictChunk) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	direct, viaEnt, free := uniqueID("chunk"), uniqueID("chunk"), uniqueID("chunk")
	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{direct, viaEnt, free} {
		if err := seed(ctx, id, project, "content of "+id, false, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if got, err := screen.RestrictedChunks(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("empty input: %v %v", got, err)
	}
	liftDirect, err := restrict(ctx, direct, project, false)
	if err != nil {
		t.Fatalf("restrict direct: %v", err)
	}
	liftEnt, err := restrict(ctx, viaEnt, project, true)
	if err != nil {
		t.Fatalf("restrict via entity: %v", err)
	}
	ent := RestrictedEntityID(viaEnt)
	unrelatedEnt := uniqueID("ent")

	chunks, err := screen.RestrictedChunks(ctx, []string{direct, viaEnt, free})
	if err != nil {
		t.Fatalf("RestrictedChunks: %v", err)
	}
	if !chunks[direct] || !chunks[viaEnt] || chunks[free] || len(chunks) != 2 {
		t.Errorf("RestrictedChunks = %v, want exactly the direct and via-entity chunks", chunks)
	}
	ents, err := screen.RestrictedEntities(ctx, []string{ent, unrelatedEnt})
	if err != nil {
		t.Fatalf("RestrictedEntities: %v", err)
	}
	if !ents[ent] || ents[unrelatedEnt] || len(ents) != 1 {
		t.Errorf("RestrictedEntities = %v, want exactly the linked entity %s", ents, ent)
	}

	if err := liftDirect(); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if err := liftEnt(); err != nil {
		t.Fatalf("lift: %v", err)
	}
	chunks, err = screen.RestrictedChunks(ctx, []string{direct, viaEnt, free})
	if err != nil || len(chunks) != 0 {
		t.Errorf("after the lift RestrictedChunks = %v (%v), want none", chunks, err)
	}
	ents, err = screen.RestrictedEntities(ctx, []string{ent})
	if err != nil || len(ents) != 0 {
		t.Errorf("after the lift RestrictedEntities = %v (%v), want none", ents, err)
	}
}
