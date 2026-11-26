package retention

import "testing"

// Review 6a1e of GitHub #470 (GDPR design §4.22): the Art 15 report
// re-implemented the sweeper's zero-means-default rule for four windows. Both
// now read these functions; this pins the rule itself.
func TestEffectiveWindows_ZeroMeansTheCompiledDefault(t *testing.T) {
	var g GlobalPolicy
	for name, got := range map[string][2]int{
		"execution_ratings":    {g.EffectiveExecutionRatingsDays(), DefaultExecutionRatingsDays},
		"config_apply_journal": {g.EffectiveConfigApplyJournalDays(), DefaultConfigApplyJournalDays},
		"blackbox_trace_cache": {g.EffectiveBlackboxTraceCacheDays(), DefaultBlackboxTraceCacheDays},
		"project_gists":        {g.EffectiveProjectGistsDays(), DefaultProjectGistsDays},
		"graph_quarantine":     {Policy{}.EffectiveGraphQuarantineDays(), DefaultGraphQuarantineDays},
	} {
		if got[0] != got[1] {
			t.Errorf("%s: zero must mean the default %d, got %d", name, got[1], got[0])
		}
	}
	g = GlobalPolicy{ExecutionRatingsDays: 9, ConfigApplyJournalDays: 8, BlackboxTraceCacheDays: 7, ProjectGistsDays: 6}
	if g.EffectiveExecutionRatingsDays() != 9 || g.EffectiveConfigApplyJournalDays() != 8 ||
		g.EffectiveBlackboxTraceCacheDays() != 7 || g.EffectiveProjectGistsDays() != 6 {
		t.Errorf("a configured window must be kept, got %+v", g)
	}
	if (GlobalPolicy{ExecutionRatingsDays: -1}).EffectiveExecutionRatingsDays() != DefaultExecutionRatingsDays {
		t.Error("a negative ratings window means the default, as the sweeper always applied it")
	}
	if (Policy{GraphQuarantineDays: -1}).EffectiveGraphQuarantineDays() != -1 {
		t.Error("a negative graph quarantine is the legal hold and must be returned as it is")
	}
}
