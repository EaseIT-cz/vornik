package repotest

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// LinkChunk links subjectID to a memory chunk with the given link source,
// creating whatever parent rows the driver's foreign keys need. Drivers without
// a subject writer seed the link table directly.
type LinkChunk func(ctx context.Context, subjectID, chunkID, projectID, source string) error

// RunSubjectTextSearchSuite pins persistence.SQLSubjectTextSearch — the GDPR
// §5.1 free-text backstop of the console resolution page — on both drivers.
//
// GitHub #345 (2026-10-12): a person the knowledge-graph extractor never turned
// into an entity was reachable only by the operator's own SQL. The search must
// match case-insensitively on the literal term, treat LIKE wildcards in the
// term as literals, honour the project and chunk filters, report this subject's
// existing link and how many OTHER subjects link the row, and NEVER return a
// chunk under an active Art 18 restriction (either route) — returning it again
// once the restriction is lifted.
//
// see LLD § https://docs.vornik.io §4.17
func RunSubjectTextSearchSuite(t *testing.T, s persistence.SubjectTextSearch, seed SeedChunk, restrict RestrictChunk, link LinkChunk) {
	t.Helper()
	ctx := context.Background()
	projA, projB := uniqueID("proj"), uniqueID("proj")
	subject, other := uniqueID("ds"), uniqueID("ds")
	marker := strings.ReplaceAll(uniqueID("Zelda"), ".", "")
	ids := map[string]string{}
	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	chunks := []struct{ key, project, content string }{
		{"upper", projA, "Met " + strings.ToUpper(marker) + " at the station"},
		{"lower", projA, "note: " + strings.ToLower(marker) + " called"},
		{"projB", projB, "Lunch with " + marker + " on Friday"},
		{"none", projA, "nothing about anyone"},
		{"direct", projA, "restricted mention of " + marker},
		{"viaEnt", projA, "restricted through an entity: " + marker},
		{"wild", projA, "literal 100%_" + marker + " text"},
	}
	for i, c := range chunks {
		id := uniqueID("chunk-" + c.key)
		ids[c.key] = id
		if err := seed(ctx, id, c.project, c.content, false, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("seed %s: %v", c.key, err)
		}
	}
	if err := link(ctx, subject, ids["lower"], projA, "operator_asserted"); err != nil {
		t.Fatalf("link here: %v", err)
	}
	if err := link(ctx, other, ids["lower"], projA, "kg_extraction"); err != nil {
		t.Fatalf("link other: %v", err)
	}
	liftDirect, err := restrict(ctx, ids["direct"], projA, false)
	if err != nil {
		t.Fatalf("restrict direct: %v", err)
	}
	liftEnt, err := restrict(ctx, ids["viaEnt"], projA, true)
	if err != nil {
		t.Fatalf("restrict via entity: %v", err)
	}

	find := func(q persistence.SubjectTextQuery) map[string]persistence.SubjectTextMention {
		t.Helper()
		q.SubjectID = subject
		got, err := s.FindMentions(ctx, q)
		if err != nil {
			t.Fatalf("FindMentions(%+v): %v", q, err)
		}
		out := map[string]persistence.SubjectTextMention{}
		for _, m := range got {
			out[m.ChunkID] = m
		}
		return out
	}

	checkSubjectTextMatches(t, find(persistence.SubjectTextQuery{Term: marker, Limit: 50}), ids, marker, projA)
	checkSubjectTextFilters(t, find, ids, marker, projA, projB)
	if got, err := s.FindMentions(ctx, persistence.SubjectTextQuery{SubjectID: subject, Term: "  ", Limit: 50}); err == nil || len(got) != 0 {
		t.Errorf("a blank term must be refused, never match every row: %d rows, err=%v", len(got), err)
	}

	if err := liftDirect(); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if err := liftEnt(); err != nil {
		t.Fatalf("lift: %v", err)
	}
	after := find(persistence.SubjectTextQuery{Term: marker, Limit: 50})
	if _, ok := after[ids["direct"]]; !ok {
		t.Error("after the lift the directly linked chunk must be searchable again")
	}
	if _, ok := after[ids["viaEnt"]]; !ok {
		t.Error("after the lift the entity-route chunk must be searchable again")
	}
}

// checkSubjectTextMatches pins matching, the Art 18 exclusion and the link
// columns of one unfiltered search.
func checkSubjectTextMatches(t *testing.T, all map[string]persistence.SubjectTextMention, ids map[string]string, marker, projA string) {
	t.Helper()
	for _, k := range []string{"upper", "lower", "projB", "wild"} {
		if _, ok := all[ids[k]]; !ok {
			t.Errorf("chunk %q must match %q case-insensitively", k, marker)
		}
	}
	if _, ok := all[ids["none"]]; ok {
		t.Error("a chunk without the term must not match")
	}
	if _, ok := all[ids["direct"]]; ok {
		t.Error("a chunk linked to a restricted subject was returned (Art 18)")
	}
	if _, ok := all[ids["viaEnt"]]; ok {
		t.Error("a chunk mentioning a restricted subject's entity was returned (Art 18)")
	}
	m := all[ids["lower"]]
	if m.LinkedHereSource != "operator_asserted" || m.OtherSubjects != 1 {
		t.Errorf("linked chunk: source=%q others=%d, want operator_asserted/1", m.LinkedHereSource, m.OtherSubjects)
	}
	if m.ProjectID != projA || !strings.Contains(m.Content, strings.ToLower(marker)) || m.CreatedAt == "" {
		t.Errorf("mention fields not populated: %+v", m)
	}
	if u := all[ids["upper"]]; u.LinkedHereSource != "" || u.OtherSubjects != 0 {
		t.Errorf("unlinked chunk reports a link: %+v", u)
	}
}

// checkSubjectTextFilters pins the project and chunk filters, wildcard
// escaping and the limit.
func checkSubjectTextFilters(t *testing.T, find func(persistence.SubjectTextQuery) map[string]persistence.SubjectTextMention,
	ids map[string]string, marker, projA, projB string) {
	t.Helper()
	if got := find(persistence.SubjectTextQuery{Term: marker, ProjectID: projB, Limit: 50}); len(got) != 1 || got[ids["projB"]].ChunkID == "" {
		t.Errorf("project filter: got %d rows, want only projB's", len(got))
	}
	if got := find(persistence.SubjectTextQuery{Term: marker, ChunkID: ids["upper"], Limit: 50}); len(got) != 1 {
		t.Errorf("chunk point filter: got %d rows, want 1", len(got))
	}
	if got := find(persistence.SubjectTextQuery{Term: "nothing about", ChunkID: ids["upper"], Limit: 50}); len(got) != 0 {
		t.Error("the point filter must still require the term to match")
	}
	// Wildcards in the term are literals: "%_" matches only the chunk that
	// literally contains it, never every row.
	if got := find(persistence.SubjectTextQuery{Term: "100%_", ProjectID: projA, Limit: 50}); len(got) != 1 || got[ids["wild"]].ChunkID == "" {
		t.Errorf("wildcard term: got %d rows, want only the literal match", len(got))
	}
	if got := find(persistence.SubjectTextQuery{Term: "%", ProjectID: projA, Limit: 50}); len(got) != 1 {
		t.Errorf("a bare %% must match only rows literally containing it, got %d", len(got))
	}
	if got := find(persistence.SubjectTextQuery{Term: marker, Limit: 2}); len(got) != 2 {
		t.Errorf("limit: got %d rows, want 2", len(got))
	}
}
