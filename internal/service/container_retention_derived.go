package service

import (
	"time"

	"vornik.io/vornik/internal/retention"
)

// logDerivedCopySweep prunes the vornik-update.sh database dumps and logs one
// line per sweeper tick for the three derived copies of transcript text
// (GitHub #394, retention design 2026-10-12-tool-audit-retention-floor §8.2.4):
// examined and pruned for blackbox_trace_cache and project_gists (from the
// global sweep just run), and examined, pruned, kept and ignored for the dumps.
//
// The line is logged even when nothing was pruned: a count of zero without its
// denominator cannot tell "examined and clean" from "never examined"
// (CLAUDE.md §4).
func (c *Container) logDerivedCopySweep(g retention.GlobalCounts, dumps retention.UpgradeDumpPolicy) {
	d, err := retention.PruneUpgradeDumps(dumps, time.Now(), true)
	ev := c.Logger.Info()
	if err != nil || d.Errors > 0 {
		ev = c.Logger.Warn().Err(err).Int("upgrade_dump_errors", d.Errors)
	}
	ev.
		Int("blackbox_trace_cache_examined", g.BlackboxTraceCacheExamined).
		Int("blackbox_trace_cache_pruned", g.BlackboxTraceCache).
		Int("project_gists_examined", g.ProjectGistsExamined).
		Int("project_gists_pruned", g.ProjectGists).
		Int("project_gist_narratives_cleared", g.ProjectGistNarratives).
		Str("upgrade_dump_root", d.Root).
		Int("upgrade_dumps_examined", d.Examined).
		Int("upgrade_dumps_pruned", d.Pruned).
		Int("upgrade_dumps_kept", d.Kept).
		Int("upgrade_dumps_ignored", d.Ignored).
		Msg("retention derived copies")
}
