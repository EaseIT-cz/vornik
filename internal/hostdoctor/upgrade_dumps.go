package hostdoctor

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"vornik.io/vornik/internal/retention"
)

// WithUpgradeDumps sets the upgrade-dump policy the upgrade_dumps check reads
// (from the daemon config). Without it the check uses the compiled defaults
// and the current user's home.
func (h *Checker) WithUpgradeDumps(p retention.UpgradeDumpPolicy) *Checker {
	h.upgradeDumps = &p
	return h
}

// checkUpgradeDumps reports the database dumps vornik-update.sh left behind
// against the retention policy (GitHub #394, retention design §8.2.3). It runs
// the pruner in PREVIEW — the doctor never deletes — and reads files only, so
// it spawns no process. The message always carries the denominator: "0
// examined" is said as such, never as a bare OK.
func (h *Checker) checkUpgradeDumps(_ context.Context) Check {
	const name = "upgrade_dumps"
	var p retention.UpgradeDumpPolicy
	if h.upgradeDumps != nil {
		p = *h.upgradeDumps
	}
	now := time.Now()
	if h.nowFunc != nil {
		now = h.nowFunc()
	}
	c, err := retention.PruneUpgradeDumps(p, now, false)
	if err != nil {
		return Check{Name: name, Status: "SKIPPED", Message: "upgrade dumps not examined: " + err.Error()}
	}
	policy := fmt.Sprintf("keep %d, grace %d days", c.Keep, c.GraceDays)
	if c.Pruned == 0 {
		return Check{Name: name, Status: "OK", Message: fmt.Sprintf(
			"%d dump(s) examined in %s, none past the policy (%s)", c.Examined, c.Root, policy)}
	}
	var items []string
	for _, d := range c.Decisions {
		if d.Pruned {
			items = append(items, fmt.Sprintf("%s (%s)", filepath.Join(d.Dir, d.File), d.Reason))
		}
	}
	return Check{Name: name, Status: "WARNING", Items: items, Message: fmt.Sprintf(
		"%d of %d dump(s) in %s are past the policy (%s). Each is a full copy of the database, "+
			"so erasures made since are undone in it; run vornikctl retention upgrade-dumps --apply",
		c.Pruned, c.Examined, c.Root, policy)}
}
