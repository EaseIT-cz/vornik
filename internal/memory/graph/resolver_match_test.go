package graph

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #419 (knowledge-graph LLD §3.3c, found in production): the
// resolver copied the model's match_id into Resolution.MatchID unverified;
// damaged ids (a catalog id with a digit dropped) failed the mention insert
// with an FK violation reported as "not found". Unit companions of the
// driver-contract suite repotest.RunGraphPipelineResolverMatchSuite.
func TestResolver_matchIDMustNameAnEntityOfTheProject(t *testing.T) {
	shown := &persistence.KnowledgeEntity{ID: "kent_aaaaaaaaaaaaaaaa", ProjectID: "proj", CanonicalName: "ACME Plumbing Inc", Type: "VENDOR"}
	outside := &persistence.KnowledgeEntity{ID: "kent_bbbbbbbbbbbbbbbb", ProjectID: "proj", CanonicalName: "Rolety", Type: "VENDOR"}
	foreign := &persistence.KnowledgeEntity{ID: "kent_cccccccccccccccc", ProjectID: "other", CanonicalName: "Rolety", Type: "VENDOR"}

	cases := []struct {
		name, matchID, wantDecision, wantID string
		wantInvalid                         int
		wantGet                             bool
	}{
		{"shown in the catalog: kept without a query", shown.ID, "match", shown.ID, 0, false},
		{"padded with whitespace: trimmed and kept", "  " + shown.ID + " ", "match", shown.ID, 0, false},
		{"real entity of the project outside the catalog: kept", outside.ID, "match", outside.ID, 0, true},
		{"catalog id with a digit dropped: downgraded", shown.ID[:len(shown.ID)-1], "ambiguous", "", 1, true},
		{"empty: downgraded", "", "ambiguous", "", 1, false},
		{"another project's entity: downgraded", foreign.ID, "ambiguous", "", 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeEntityRepo{
				similar: []*persistence.KnowledgeEntity{shown},
				byID:    map[string]*persistence.KnowledgeEntity{outside.ID: outside, foreign.ID: foreign, shown.ID: shown},
			}
			llmReply := `[{"candidate_id":"cand-0","decision":"match","match_id":"` + tc.matchID + `"}]`
			r := NewResolver(&fakeProvider{replies: []reply{{content: llmReply}}}, "fake", repo, fakeEmbedder)
			res, m, err := r.Resolve(context.Background(), "proj", []Candidate{{Type: "VENDOR", Name: "ACME Blinds"}})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res[0].Decision != tc.wantDecision || res[0].MatchID != tc.wantID {
				t.Fatalf("got %s %q, want %s %q", res[0].Decision, res[0].MatchID, tc.wantDecision, tc.wantID)
			}
			if tc.wantInvalid == 1 && res[0].Reason != ReasonInvalidMatchID {
				t.Errorf("reason = %q, want %q", res[0].Reason, ReasonInvalidMatchID)
			}
			if m.InvalidMatchIDs != tc.wantInvalid {
				t.Errorf("InvalidMatchIDs = %d, want %d", m.InvalidMatchIDs, tc.wantInvalid)
			}
			if got := len(repo.gets) > 0; got != tc.wantGet {
				t.Errorf("Get called = %v, want %v (gets %v)", got, tc.wantGet, repo.gets)
			}
		})
	}
}
