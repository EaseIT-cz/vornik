// Package gitbackup holds the two best-effort steps every `reset --hard` of a
// project clone runs first, so a reset never destroys work it cannot give
// back: local-only commits and tracked uncommitted changes are kept under
// refs/vornik/discarded/ (executor design 06-executor.md §9.6.6 and §9.6.7,
// GitHub #484 and #675, operator decision A10).
//
// The executor's task-start resets and the autonomy backlog-mode refresh both
// call it, so the three resets share one implementation of the rule. Neither
// step ever blocks the reset: a failure is a DEBUG log line.
package gitbackup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Run executes one git command in the clone (the caller supplies the
// directory and the process-spawn law's GitWorkspace kind) and returns its
// stdout.
type Run func(ctx context.Context, args ...string) ([]byte, error)

// LocalOnlyCommits keeps the commits on HEAD that origin/<branch> does not
// have in refs/vornik/discarded/<unix>-<short HEAD>.
func LocalOnlyCommits(ctx context.Context, run Run, branch string, logger zerolog.Logger) {
	countOut, err := run(ctx, "rev-list", "--count", "origin/"+branch+"..HEAD")
	if err != nil {
		logger.Debug().Err(err).Str("branch", branch).
			Msg("reset to origin: rev-list ahead-count failed — continuing to reset")
		return
	}
	count := 0
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(countOut)), "%d", &count); scanErr != nil || count == 0 {
		return // no local-only commits — nothing to back up
	}
	shortOut, err := run(ctx, "rev-parse", "--short", "HEAD")
	if err != nil {
		logger.Debug().Err(err).
			Msg("reset to origin: could not resolve short HEAD for backup ref — continuing anyway")
		return
	}
	backupRef := fmt.Sprintf("refs/vornik/discarded/%d-%s", time.Now().Unix(), strings.TrimSpace(string(shortOut)))
	if _, err := run(ctx, "update-ref", backupRef, "HEAD"); err != nil {
		logger.Debug().Err(err).Str("branch", branch).
			Msg("reset to origin: could not create backup ref for local-only commits — continuing anyway")
		return
	}
	logger.Warn().
		Str("branch", branch).
		Int("count", count).
		Str("backup_ref", backupRef).
		Msg("reset to origin: local HEAD has commits not present on origin — resetting to origin; commits saved to backup ref and are recoverable from it")
}

// DirtyTrackedChanges keeps the index and the tracked working-tree changes in
// refs/vornik/discarded/<unix>-dirty-<short id>, via `git stash create`
// (which touches no ref, index entry or file; untracked files are not
// included). Recover with `git stash apply <ref>`.
//
// ignore, when not empty, is a path relative to the clone whose change alone
// is no reason to back up: the backlog file, whose [x]/[!] marks the backlog
// store re-applies after every reset (§9.6.7). When another tracked path is
// dirty too, the backup keeps the whole tracked tree, that file included.
func DirtyTrackedChanges(ctx context.Context, run Run, ignore string, logger zerolog.Logger) {
	st, err := run(ctx, "status", "--porcelain", "-uno")
	if err != nil {
		logger.Debug().Err(err).
			Msg("reset to origin: git status failed — tracked uncommitted changes are not backed up")
		return
	}
	count, other := 0, false
	for _, line := range strings.Split(string(st), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		count++
		if ignore == "" || porcelainPath(line) != ignore {
			other = true
		}
	}
	if !other {
		return // nothing dirty, or only the ignored file
	}
	out, err := run(ctx, "-c", "user.name=vornik-agent", "-c", "user.email=agent@vornik.io", "stash", "create")
	if err != nil {
		logger.Debug().Err(err).
			Msg("reset to origin: git stash create failed — tracked uncommitted changes are not backed up")
		return
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return
	}
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	backupRef := fmt.Sprintf("refs/vornik/discarded/%d-dirty-%s", time.Now().Unix(), short)
	if _, err := run(ctx, "update-ref", backupRef, sha); err != nil {
		logger.Debug().Err(err).
			Msg("reset to origin: could not create the backup ref for tracked uncommitted changes — they are not backed up")
		return
	}
	logger.Warn().
		Int("count", count).
		Str("backup_ref", backupRef).
		Msg("reset to origin: the clone had tracked uncommitted changes — resetting; they are saved to the backup ref (recover with git stash apply <ref>)")
}

// porcelainPath is the path of one `git status --porcelain` line ("XY path",
// or "XY old -> new" for a rename, which names the new path). A quoted path
// (one with special characters) never equals a plain ignore path, so it
// counts as another change: the safe direction.
func porcelainPath(line string) string {
	if len(line) < 4 {
		return ""
	}
	p := line[3:]
	if i := strings.Index(p, " -> "); i >= 0 {
		p = p[i+4:]
	}
	return p
}
