package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Found-at level (operator, 2026-10-14; ENGINEERING-TENETS §4.7): a bug's
// regression test sits at the level where the bug was found, so a deposit
// records that level while its finder still knows it. The endpoint takes an
// optional found_at from the tenets' vocabulary and renders it first inside
// the trailer, where neither the title parse nor the cooldown date match
// reads (autonomous-dev-loop design §C1, amendment 2026-10-14).

func TestBacklogDeposit_FoundAt_RenderedFirstInTrailer(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req := baseDepositReq("task-1", "Fix the widget cache")
	req.FoundAt = "e2e-lane"
	rec := postBacklogDeposit(t, fx.server, req, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "accepted", resp.Status)
	require.Contains(t, resp.Item, "**[bug]** Fix the widget cache — ")
	require.Contains(t, resp.Item, "(found at: e2e-lane; evidence: log line 42; via task-1,")
	require.Contains(t, fx.readBacklog(t), "- [?] "+resp.Item)
}

func TestBacklogDeposit_FoundAt_WithoutEvidence(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req := baseDepositReq("task-1", "a finding with a level and no evidence")
	req.Evidence = ""
	req.FoundAt = "review"
	resp := decodeBacklogDepositResponse(t, postBacklogDeposit(t, fx.server, req, nil))
	require.Equal(t, "accepted", resp.Status)
	require.Regexp(t, `\(found at: review; via task-1, \d{4}-\d{2}-\d{2}\)$`, resp.Item)
}

func TestBacklogDeposit_FoundAt_EveryLevelAccepted(t *testing.T) {
	for _, lvl := range []string{"unit", "integration", "executor-e2e", "e2e-lane", "production", "review"} {
		t.Run(lvl, func(t *testing.T) {
			fx := newBacklogDepositFixture(t, "", nil, nil)
			req := baseDepositReq("task-1", "finding at level "+lvl)
			req.FoundAt = lvl
			resp := decodeBacklogDepositResponse(t, postBacklogDeposit(t, fx.server, req, nil))
			require.Equal(t, "accepted", resp.Status)
			require.Contains(t, resp.Item, "(found at: "+lvl+"; ")
		})
	}
}

func TestBacklogDeposit_FoundAt_UnknownValueRejectedAndNothingWritten(t *testing.T) {
	for _, bad := range []string{"staging", "Production", "e2e", "unit; via task-9, 2020-01-01)"} {
		t.Run(bad, func(t *testing.T) {
			fx := newBacklogDepositFixture(t, "", nil, nil)
			req := baseDepositReq("task-1", "a finding with a bad level")
			req.FoundAt = bad
			rec := postBacklogDeposit(t, fx.server, req, nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			require.Contains(t, rec.Body.String(), "found_at must be one of")
			require.NotContains(t, fx.readBacklog(t), "a finding with a bad level")
		})
	}
}

func TestBacklogDeposit_FoundAt_AbsentRendersAsBefore(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	resp := decodeBacklogDepositResponse(t, postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "Fix the widget cache"), nil))
	require.Equal(t, "accepted", resp.Status)
	require.NotContains(t, resp.Item, "found at")
	require.Contains(t, resp.Item, "(evidence: log line 42; via task-1,")
}

// The trailer change must not weaken dedup or the cooldown: an open item
// carrying a found-at trailer still dedups by title, and a closed one still
// yields its date to the cooldown.
func TestBacklogDeposit_FoundAt_DedupAndCooldownUnchanged(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	first := baseDepositReq("task-1", "Fix the widget cache")
	first.FoundAt = "integration"
	require.Equal(t, "accepted", decodeBacklogDepositResponse(t, postBacklogDeposit(t, fx.server, first, nil)).Status)
	again := decodeBacklogDepositResponse(t, postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "Fix The Widget Cache!"), nil))
	require.Equal(t, "duplicate", again.Reason)

	fx2 := newBacklogDepositFixture(t, "", nil, nil)
	recent := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
	seedBacklogFile(t, fx2.backlogPath(t), []string{
		fmt.Sprintf("- [x] **[bug]** flaky retry loop — missing jitter (found at: production; evidence: x.go:1; via task-0, %s)", recent),
	})
	req := baseDepositReq("task-1", "flaky retry loop")
	req.Regression = true
	req.FoundAt = "production"
	resp := decodeBacklogDepositResponse(t, postBacklogDeposit(t, fx2.server, req, nil))
	require.Equal(t, "cooldown", resp.Reason)
}
