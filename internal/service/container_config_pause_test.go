package service

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #302 (2026-10-13): the dispatch-refused reason named neither the
// journal row nor how to clear it, so the last_error an agent read said only
// "journal DRIFT" (config-apply journal design §13.1). The gate's reason now
// names the journal id (§13.2).
func TestConfigBlockedGate_ReasonNamesTheJournalID(t *testing.T) {
	c := newProposalApplierContainer(t)
	ctx := context.Background()
	row := &persistence.ConfigApplyJournal{
		ID:               "caj_20261013090000_00000000000000a1",
		ProposalID:       "prop-302",
		OpDigest:         "sha256:ops",
		DeploymentRoot:   "/deploy/configs",
		AffectedProjects: []string{"hermes--global-news"},
		Ops:              []persistence.JournalOp{{Op: "replace", Path: "projects/hermes--global-news.yaml", ContentSHA256: "aa"}},
		PreImages:        map[string]persistence.JournalPreImage{"projects/hermes--global-news.yaml": {Existed: true, SHA256: "bb", Content: "x"}},
		ReadSet:          map[string]string{},
	}
	if err := c.repos.ApplyJournal.Prepare(ctx, row); err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{persistence.JournalStatePrepared, persistence.JournalStateReverting},
		{persistence.JournalStateReverting, persistence.JournalStateDrift},
	} {
		if err := c.repos.ApplyJournal.Transition(ctx, row.ID, step[0], step[1],
			persistence.JournalPatch{Failure: "drift: projects/hermes--global-news.yaml is absent; matches neither pre-image nor intended content"}); err != nil {
			t.Fatalf("%s→%s: %v", step[0], step[1], err)
		}
	}

	gate := c.configApplyBlockedGate()
	blocked, reason := gate("hermes--global-news")
	if !blocked {
		t.Fatal("a DRIFT row must block its project")
	}
	if !strings.Contains(reason, row.ID) {
		t.Errorf("reason %q does not name the journal row %s", reason, row.ID)
	}
	if blocked, _ := gate("other"); blocked {
		t.Error("a project the row does not name must not be blocked")
	}
}
