package api

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #382, companion RAG-first guidance design §11.2. Found at:
// production observation, 2026-10-15: recall("deployment architecture
// design", repo_scope=<a sparsely documented sibling repository>) returned 10
// hits, 8 spec/decision-class, all 8 NULL-scoped Vornik designs, 0 in that
// repository's scope, under retrieval_trust_verdict "high". A session told
// "the design/spec/decision chunks are the authoritative design record" read
// another repository's designs as its own. Regression test at: the MCP
// tools/call handler, the level a client observes.

// A neutral stand-in for the sibling repository: this file ships in the
// Community export, whose gates refuse private repository paths.
const infraScope = "github.com/example/sparse-infra"

func recallDesignRecord(t *testing.T, hits []MemorySearchResult, args map[string]any) (map[string]any, bool) {
	t.Helper()
	return recallDesignRecordWithKeyScope(t, "", hits, args)
}

func recallDesignRecordWithKeyScope(t *testing.T, keyScope string, hits []MemorySearchResult, args map[string]any) (map[string]any, bool) {
	t.Helper()
	return recallDesignRecordWith(t, keyScope, &fakeMemoryCompanion{recallReturn: hits}, args)
}

func recallDesignRecordWith(t *testing.T, keyScope string, fake *fakeMemoryCompanion, args map[string]any) (map[string]any, bool) {
	t.Helper()
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = fake
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)
	keyRepo.rows[len(keyRepo.rows)-1].DefaultRepoScope = keyScope
	req := withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{"name": "recall", "arguments": args}), raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	dr, ok := out["design_record"].(map[string]any)
	return dr, ok
}

func hit(id, class, scope string) MemorySearchResult {
	return MemorySearchResult{ChunkID: id, ProjectID: "alpha", SourceName: id, Content: "c", Score: 0.5, ContentClass: class, RepoScope: scope}
}

// The measured shape: NULL-scoped designs and a cross-cutting spec do not
// count; an in-scope research note does not count either.
func TestCompanionRecall_DesignRecord_LeakedDesignsDoNotCount(t *testing.T) {
	hits := []MemorySearchResult{
		hit("a", "spec", ""), hit("b", "decision", ""), hit("c", "SPEC", ""),
		hit("d", "spec", "*"), hit("e", "research", infraScope),
	}
	dr, ok := recallDesignRecord(t, hits, map[string]any{"query": "deployment architecture design", "repo_scope": infraScope})
	require.True(t, ok, "every scoped recall carries design_record")
	assert.Equal(t, infraScope, dr["repo_scope"])
	assert.EqualValues(t, 5, dr["examined"], "the denominator is the hits returned")
	assert.EqualValues(t, 0, dr["design_hits"])
	assert.Equal(t, designRecordNoneNote(infraScope, 5), dr["note"])
	assert.Contains(t, dr["note"], "Do not treat other hits as its design")
}

func TestCompanionRecall_DesignRecord_InScopeDesignCounts(t *testing.T) {
	hits := []MemorySearchResult{hit("a", "Spec", infraScope), hit("b", "decision", infraScope), hit("c", "spec", "")}
	dr, ok := recallDesignRecord(t, hits, map[string]any{"query": "q", "repo_scope": infraScope})
	require.True(t, ok)
	assert.EqualValues(t, 3, dr["examined"])
	assert.EqualValues(t, 2, dr["design_hits"])
	assert.Equal(t, designRecordSomeNote(infraScope, 2, 3), dr["note"])
}

// No effective scope (the Hermes memory provider passes none): the recall is
// project-wide, so every design-class hit counts, NULL and * included, and
// the record carries no note. GitHub #387, design §11.12. Found at: review of
// the Hermes e2e lane run on plugin 0.22.0 (2026-10-15); a user observes it at
// e2e-lane level, where a personal question ("who is my dentist") came back
// with "memory holds no design for this question in this repository ... offer
// to write an as-built record". Regression test at: this MCP tools/call
// handler, plus the lane's H3g arm (memory_visible_lane_test.go).
func TestCompanionRecall_DesignRecord_NoScopeHasNoNote(t *testing.T) {
	hits := []MemorySearchResult{hit("a", "spec", ""), hit("b", "decision", "*"), hit("c", "research", "x")}
	dr, ok := recallDesignRecord(t, hits, map[string]any{"query": "who is my dentist"})
	require.True(t, ok, "the project-wide count stays: the Hermes develop-with-memory skill reads it")
	assert.Equal(t, "", dr["repo_scope"])
	assert.EqualValues(t, 3, dr["examined"])
	assert.EqualValues(t, 2, dr["design_hits"])
	_, hasNote := dr["note"]
	assert.False(t, hasNote, "coding guidance must not reach an unscoped (personal-assistant) recall: %v", dr["note"])

	dr, ok = recallDesignRecord(t, nil, map[string]any{"query": "who is my dentist"})
	require.True(t, ok)
	_, hasNote = dr["note"]
	assert.False(t, hasNote, "nor when the unscoped recall returned nothing")
}

// Review eab1 F3: with nothing examined, nothing is ruled out.
func TestCompanionRecall_DesignRecord_ZeroHitsIsUndetermined(t *testing.T) {
	dr, ok := recallDesignRecord(t, nil, map[string]any{"query": "q", "repo_scope": infraScope})
	require.True(t, ok)
	assert.EqualValues(t, 0, dr["examined"])
	assert.EqualValues(t, 0, dr["design_hits"])
	assert.Equal(t, designRecordUndeterminedNote, dr["note"])
	assert.NotContains(t, dr["note"], "holds no design")
}

// A class filter that cannot return designs makes a zero meaningless, so the
// field is left out; a design-class filter keeps it.
func TestCompanionRecall_DesignRecord_ClassFilter(t *testing.T) {
	hits := []MemorySearchResult{hit("a", "companion_note", infraScope), hit("b", "spec", infraScope)}
	_, ok := recallDesignRecord(t, hits, map[string]any{"query": "q", "repo_scope": infraScope, "class": "companion_note"})
	assert.False(t, ok, "a non-design class filter omits design_record")
	dr, ok := recallDesignRecord(t, hits, map[string]any{"query": "q", "repo_scope": infraScope, "class": "spec"})
	require.True(t, ok)
	assert.EqualValues(t, 1, dr["examined"])
	assert.EqualValues(t, 1, dr["design_hits"])
}

// Review 7690 F4: a key that carries a default scope, called without
// repo_scope, counts against that default (effectiveRepoScope), never
// project-wide, or the measured leak would come back through the default path.
func TestCompanionRecall_DesignRecord_KeyDefaultScope(t *testing.T) {
	hits := []MemorySearchResult{hit("a", "spec", ""), hit("b", "spec", infraScope)}
	dr, ok := recallDesignRecordWithKeyScope(t, infraScope, hits, map[string]any{"query": "q"})
	require.True(t, ok)
	assert.Equal(t, infraScope, dr["repo_scope"])
	assert.EqualValues(t, 1, dr["design_hits"], "the NULL-scoped spec must not count under the key's default scope")
}

// Review 7690 F3: examined is the hits returned after min_score, so a design
// hit the caller filtered out is neither examined nor counted.
func TestCompanionRecall_DesignRecord_ExaminedIsAfterMinScore(t *testing.T) {
	low := hit("low", "spec", infraScope)
	low.Score = 0.1
	high := hit("high", "research", infraScope)
	high.Score = 0.9
	dr, ok := recallDesignRecord(t, []MemorySearchResult{low, high}, map[string]any{"query": "q", "repo_scope": infraScope, "min_score": 0.5})
	require.True(t, ok)
	assert.EqualValues(t, 1, dr["examined"])
	assert.EqualValues(t, 0, dr["design_hits"])
}

// workflowHit is an in-scope hit a task produced: a review or findings
// artifact ingested with its task's id.
func workflowHit(id, class string) MemorySearchResult {
	h := hit(id, class, infraScope)
	h.TaskID = "task_" + id
	return h
}

// GitHub #390, design §11.14. Found at: production observation, 2026-10-15
// (#382 measurement, §11.13): after two companion-doc-review rounds and before
// the reviewed record was stored, a vornik-infra surface recall returned 10
// decision-class review chunks, each carrying its task id, and design_record
// called them "this repository's design record". Regression test at: the MCP
// tools/call handler, where a client reads the note.
func TestCompanionRecall_DesignRecord_WorkflowOutputIsNotADesign(t *testing.T) {
	var hits []MemorySearchResult
	for i := 0; i < 10; i++ {
		hits = append(hits, workflowHit(string(rune('a'+i)), "decision"))
	}
	dr, ok := recallDesignRecord(t, hits, map[string]any{"query": "terraform deployer IAM permissions boundary role", "repo_scope": infraScope})
	require.True(t, ok)
	assert.EqualValues(t, 0, dr["design_hits"], "a review is not the design it reviews")
	assert.EqualValues(t, 10, dr["workflow_output"])
	assert.Contains(t, dr["note"], "no design for this question was found")
	assert.Contains(t, dr["note"], designRecordWorkflowSuffix(10))
	assert.NotContains(t, dr["note"], "these are this repository's design record")

	// After the reviewed record is ingested (uploaded: no task id), its
	// chunks count and the reviews still do not: the measured 5 + 5.
	hits = hits[:5]
	for i := 0; i < 5; i++ {
		hits = append(hits, hit(string(rune('p'+i)), "spec", infraScope))
	}
	dr, ok = recallDesignRecord(t, hits, map[string]any{"query": "q", "repo_scope": infraScope})
	require.True(t, ok)
	assert.EqualValues(t, 5, dr["design_hits"])
	assert.EqualValues(t, 5, dr["workflow_output"])
	assert.Equal(t, designRecordSomeNote(infraScope, 5, 10)+" "+designRecordWorkflowSuffix(5), dr["note"])
}

// GitHub #391, design §11.14. Found at: production observation, 2026-10-15
// (#382 measurement): recall("deployment architecture design",
// repo_scope=vornik-infra) returned 10 hits, none in scope, most NULL-scoped,
// and the note said "memory holds no design for this question in this
// repository" while a strict_scope recall found the repository's design.
// Regression test at: the MCP tools/call handler.
func crowdedHits() []MemorySearchResult {
	var hits []MemorySearchResult
	for i := 0; i < 7; i++ {
		hits = append(hits, hit(string(rune('a'+i)), "spec", ""))
	}
	for i := 0; i < 3; i++ {
		hits = append(hits, hit(string(rune('k'+i)), "decision", "github.com/example/other"))
	}
	return hits
}

func TestCompanionRecall_DesignRecord_CrowdedZeroRecountsStrictly(t *testing.T) {
	args := map[string]any{"query": "deployment architecture design", "repo_scope": infraScope}
	fake := &fakeMemoryCompanion{recallReturn: crowdedHits(),
		strictRecallReturn: []MemorySearchResult{hit("s1", "spec", infraScope), hit("s2", "spec", infraScope), hit("s3", "research", infraScope)}}
	dr, ok := recallDesignRecordWith(t, "", fake, args)
	require.True(t, ok)
	assert.EqualValues(t, 0, dr["design_hits"], "the first count is reported as it was")
	assert.EqualValues(t, 7, dr["unscoped"])
	require.Len(t, fake.recallCalls, 2, "one strict recount, no more")
	assert.True(t, fake.recallCalls[1].Opts.StrictScope)
	assert.False(t, fake.recallCalls[1].Opts.Sufficient, "the recount makes no model call")
	assert.Equal(t, fake.recallCalls[0].Opts.Limit, fake.recallCalls[1].Opts.Limit)
	rc, ok := dr["strict_recount"].(map[string]any)
	require.True(t, ok, "strict_recount: %v", dr)
	assert.EqualValues(t, 3, rc["examined"])
	assert.EqualValues(t, 2, rc["design_hits"])
	assert.Equal(t, designRecordRecountFoundNote(infraScope, 10, 7, 2), dr["note"])
	assert.Contains(t, dr["note"], "strict_scope=true")
	assert.NotContains(t, dr["note"], "memory holds no design")

	// A recount that also meets workflow output carries the suffix too
	// (review f5eb S2).
	fake = &fakeMemoryCompanion{recallReturn: crowdedHits(),
		strictRecallReturn: []MemorySearchResult{hit("s1", "spec", infraScope), workflowHit("r1", "decision")}}
	dr, ok = recallDesignRecordWith(t, "", fake, args)
	require.True(t, ok)
	assert.Equal(t, designRecordRecountFoundNote(infraScope, 10, 7, 1)+" "+designRecordWorkflowSuffix(1), dr["note"])
	assert.EqualValues(t, 0, dr["workflow_output"], "the top-level field is the first count's (review f36b F2)")
	rc, ok = dr["strict_recount"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 1, rc["workflow_output"])
	assert.Equal(t, fake.recallCalls[0].Opts.RepoScope, fake.recallCalls[1].Opts.RepoScope)

	// The recount keeps the scope and dates, and its hits pass the same
	// min_score and class filters as the first search's (review f36b F3).
	low := hit("s-low", "spec", infraScope)
	low.Score = 0.1
	fake = &fakeMemoryCompanion{recallReturn: crowdedHits(),
		strictRecallReturn: []MemorySearchResult{low, hit("s-ok", "spec", infraScope)}}
	dated := map[string]any{"query": "q", "repo_scope": infraScope, "min_score": 0.3,
		"from_date": "2026-01-01T00:00:00Z", "to_date": "2026-12-31T00:00:00Z"}
	dr, ok = recallDesignRecordWith(t, "", fake, dated)
	require.True(t, ok)
	require.Len(t, fake.recallCalls, 2)
	assert.Equal(t, fake.recallCalls[0].Opts.FromDate, fake.recallCalls[1].Opts.FromDate)
	assert.Equal(t, fake.recallCalls[0].Opts.ToDate, fake.recallCalls[1].Opts.ToDate)
	assert.False(t, fake.recallCalls[1].Opts.FromDate.IsZero())
	rc, ok = dr["strict_recount"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 1, rc["examined"], "the hit below min_score is filtered from the recount too")
	assert.EqualValues(t, 1, rc["design_hits"])

	// The strict recount finds none either: the none note, from the recount.
	fake = &fakeMemoryCompanion{recallReturn: crowdedHits(), strictRecallReturn: []MemorySearchResult{hit("s1", "research", infraScope)}}
	dr, ok = recallDesignRecordWith(t, "", fake, args)
	require.True(t, ok)
	assert.Equal(t, designRecordNoneNote(infraScope, 1), dr["note"])

	// The recount fails: keep the first count and say it may be crowded.
	fake = &fakeMemoryCompanion{recallReturn: crowdedHits(), strictRecallErr: errors.New("db down")}
	dr, ok = recallDesignRecordWith(t, "", fake, args)
	require.True(t, ok)
	_, has := dr["strict_recount"]
	assert.False(t, has)
	assert.Equal(t, designRecordCrowdedNote(infraScope, 10, 7), dr["note"])

	// Review f36b F4: the suffix combines with the crowded-failure note (from
	// the first count) and with the recount-none note (from the recount).
	withReview := append(crowdedHits(), workflowHit("r0", "decision"))
	fake = &fakeMemoryCompanion{recallReturn: withReview, strictRecallErr: errors.New("db down")}
	dr, ok = recallDesignRecordWith(t, "", fake, args)
	require.True(t, ok)
	assert.Equal(t, designRecordCrowdedNote(infraScope, 11, 7)+" "+designRecordWorkflowSuffix(1), dr["note"])
	fake = &fakeMemoryCompanion{recallReturn: crowdedHits(),
		strictRecallReturn: []MemorySearchResult{workflowHit("r1", "decision"), workflowHit("r2", "spec")}}
	dr, ok = recallDesignRecordWith(t, "", fake, args)
	require.True(t, ok)
	assert.Equal(t, designRecordNoneNote(infraScope, 2)+" "+designRecordWorkflowSuffix(2), dr["note"])
}

// The recount runs only on a scoped, non-strict zero with NULL-scoped hits.
func TestCompanionRecall_DesignRecord_NoRecountWhenNotCrowded(t *testing.T) {
	cases := map[string]struct {
		hits []MemorySearchResult
		args map[string]any
	}{
		"no unscoped hits": {[]MemorySearchResult{hit("a", "research", infraScope)}, map[string]any{"query": "q", "repo_scope": infraScope}},
		"strict already":   {nil, map[string]any{"query": "q", "repo_scope": infraScope, "strict_scope": true}},
		"designs found":    {append(crowdedHits(), hit("d", "spec", infraScope)), map[string]any{"query": "q", "repo_scope": infraScope}},
		"no scope":         {crowdedHits(), map[string]any{"query": "q"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeMemoryCompanion{recallReturn: c.hits, strictRecallReturn: []MemorySearchResult{hit("s", "spec", infraScope)}}
			if c.hits == nil {
				// Review f36b F1: the strict first call itself returns a
				// crowded zero (the fake ignores strictness here), so only
				// the !strict guard keeps a second search from running.
				fake = &fakeMemoryCompanion{recallReturn: crowdedHits()}
			}
			dr, ok := recallDesignRecordWith(t, "", fake, c.args)
			require.True(t, ok)
			assert.Len(t, fake.recallCalls, 1)
			_, has := dr["strict_recount"]
			assert.False(t, has)
		})
	}
}

// Review 17b7 F7: a crowded zero on an unscoped (personal-assistant) recall
// carries no note either (§11.12), and no recount.
func TestCompanionRecall_DesignRecord_NoScopeCrowdedZeroHasNoNote(t *testing.T) {
	hits := []MemorySearchResult{hit("a", "research", ""), hit("b", "research", "")}
	fake := &fakeMemoryCompanion{recallReturn: hits}
	dr, ok := recallDesignRecordWith(t, "", fake, map[string]any{"query": "who is my dentist"})
	require.True(t, ok)
	assert.EqualValues(t, 2, dr["unscoped"], "unscoped is reported project-wide too")
	assert.EqualValues(t, dr["examined"], dr["unscoped"])
	_, hasNote := dr["note"]
	assert.False(t, hasNote)
	assert.Len(t, fake.recallCalls, 1)
}

// A zero that nothing crowded is still only "not among the M examined".
func TestCompanionRecall_DesignRecord_NoneNoteNoLongerOverclaims(t *testing.T) {
	hits := []MemorySearchResult{hit("a", "research", infraScope), hit("b", "spec", "github.com/example/other")}
	dr, ok := recallDesignRecord(t, hits, map[string]any{"query": "q", "repo_scope": infraScope})
	require.True(t, ok)
	assert.EqualValues(t, 0, dr["unscoped"])
	assert.EqualValues(t, 0, dr["workflow_output"])
	assert.NotContains(t, dr["note"], "memory holds no design")
	assert.Contains(t, dr["note"], "no design for this question was found in this repository's memory")
	assert.Contains(t, dr["note"], "offer to write an as-built record")
}
