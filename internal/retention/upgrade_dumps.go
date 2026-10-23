package retention

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Upgrade-dump retention (GitHub #394).
//
// vornik-update.sh step 1 writes $HOME/vornik-upgrade-backup-<UTC>/ holding a
// full pg_dump of the database, the previous binaries, config.yaml and
// STATE.txt. Nothing removed a dump, so every Art 17 erasure made after one
// was undone in it. This file is the ONE implementation of the policy; the
// daemon sweeper, `vornikctl retention upgrade-dumps` (which the script calls
// after a successful upgrade) and the upgrade_dumps doctor check all call
// PruneUpgradeDumps.
//
// see LLD § https://docs.vornik.io §8.2.3

const (
	// UpgradeBackupPrefix is the name prefix of a backup directory; the rest
	// of the name is the run's UTC start, upgradeBackupTimeLayout.
	UpgradeBackupPrefix     = "vornik-upgrade-backup-"
	upgradeBackupTimeLayout = "20060102T150405Z"
	// UpgradeStateFile is the key=value record the script writes per backup.
	UpgradeStateFile = "STATE.txt"
	// UpgradeCompletedKey is the STATE.txt key vornik-update.sh appends once
	// readiness passed. Its absence marks a run that did not succeed.
	UpgradeCompletedKey = "upgrade_completed_at"
	// UpgradePrunedKey is the STATE.txt key the pruner appends when it removes
	// a dump: "dump_pruned_at=<RFC 3339> reason=<keep|grace> file=<name>".
	UpgradePrunedKey  = "dump_pruned_at"
	upgradeDumpSuffix = ".dump"

	// PruneReasonKeep marks a dump beyond the newest Keep dumps.
	PruneReasonKeep = "keep"
	// PruneReasonGrace marks a dump superseded more than GraceDays ago.
	PruneReasonGrace = "grace"

	// DefaultUpgradeDumpKeep is how many dumps are kept at most.
	DefaultUpgradeDumpKeep = 2
	// DefaultUpgradeDumpGraceDays is how long a dump outlives the upgrade
	// that superseded it (its own successful completion, or a newer run).
	DefaultUpgradeDumpGraceDays = 14

	// maxStateFileBytes bounds the STATE.txt read; the script writes a few
	// hundred bytes.
	maxStateFileBytes = 64 << 10
)

// UpgradeDumpPolicy is the upgrade-dump retention policy. Zero values mean
// the defaults; there is no "keep forever" value (design §8.2.3).
type UpgradeDumpPolicy struct {
	// Root holds the vornik-upgrade-backup-* directories. Empty → the
	// current user's home, where vornik-update.sh writes them.
	Root string
	// Keep is the most dumps kept, newest first. <= 0 → DefaultUpgradeDumpKeep.
	Keep int
	// GraceDays is how long a superseded dump is kept. <= 0 →
	// DefaultUpgradeDumpGraceDays.
	GraceDays int
}

func (p UpgradeDumpPolicy) withDefaults() UpgradeDumpPolicy {
	if p.Keep <= 0 {
		p.Keep = DefaultUpgradeDumpKeep
	}
	if p.GraceDays <= 0 {
		p.GraceDays = DefaultUpgradeDumpGraceDays
	}
	return p
}

// Resolved applies the defaults, including the home-directory root.
func (p UpgradeDumpPolicy) Resolved() (UpgradeDumpPolicy, error) {
	p = p.withDefaults()
	if strings.TrimSpace(p.Root) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return p, fmt.Errorf("upgrade dumps: no root configured and no home directory: %w", err)
		}
		p.Root = home
	}
	return p, nil
}

// UpgradeDumpDecision is what the pruner decided about one dump.
type UpgradeDumpDecision struct {
	Dir     string    `json:"dir"`
	File    string    `json:"file"`
	Started time.Time `json:"started"`
	// SupersededAt is the upgrade's completion stamp, or for an unstamped run
	// the start of the next newer backup. Nil for the newest unstamped dump.
	SupersededAt *time.Time `json:"superseded_at,omitempty"`
	Pruned       bool       `json:"pruned"`
	Reason       string     `json:"reason,omitempty"`
	Error        string     `json:"error,omitempty"`
}

// UpgradeDumpCounts reports a pruner run. Examined is the denominator: every
// dump file found in the layout. Ignored counts entries that look like the
// layout but are not acted on (a symlink, a name that does not parse).
type UpgradeDumpCounts struct {
	Root      string                `json:"root"`
	Keep      int                   `json:"keep"`
	GraceDays int                   `json:"grace_days"`
	Examined  int                   `json:"examined"`
	Pruned    int                   `json:"pruned"`
	Kept      int                   `json:"kept"`
	Ignored   int                   `json:"ignored"`
	Errors    int                   `json:"errors"`
	Decisions []UpgradeDumpDecision `json:"decisions"`
}

type upgradeBackup struct {
	dir       string
	started   time.Time
	completed *time.Time
	dumps     []string
}

// PruneUpgradeDumps applies the policy under p.Root at now. With apply false
// it only decides (the doctor check and the CLI preview). A dump is pruned
// when its rank, newest first, is Keep or more, or when it was superseded
// more than GraceDays ago. The newest unstamped dump has no superseded time:
// it may be the rollback point of an upgrade that just failed.
//
// Only a regular *.dump file directly inside a real directory whose name
// parses is ever removed; symlinks are never followed and directories are
// never removed. A root that cannot be read is an error; a failure on one
// dump is recorded in its decision and the rest continue.
func PruneUpgradeDumps(p UpgradeDumpPolicy, now time.Time, apply bool) (UpgradeDumpCounts, error) {
	p, err := p.Resolved()
	counts := UpgradeDumpCounts{Root: p.Root, Keep: p.Keep, GraceDays: p.GraceDays, Decisions: []UpgradeDumpDecision{}}
	if err != nil {
		return counts, err
	}
	backups, ignored, err := scanUpgradeBackups(p.Root)
	counts.Ignored = ignored
	if err != nil {
		return counts, err
	}
	now = now.UTC()
	grace := time.Duration(p.GraceDays) * 24 * time.Hour
	rank := 0
	for i, b := range backups {
		superseded := b.completed
		if superseded == nil && i > 0 {
			newer := backups[i-1].started
			superseded = &newer
		}
		for _, file := range b.dumps {
			d := UpgradeDumpDecision{Dir: b.dir, File: file, Started: b.started, SupersededAt: superseded}
			switch {
			case rank >= p.Keep:
				d.Reason = PruneReasonKeep
			case superseded != nil && now.Sub(*superseded) > grace:
				d.Reason = PruneReasonGrace
			}
			rank++
			counts.Examined++
			// In preview a due dump counts as pruned: it is what --apply removes.
			d.Pruned = d.Reason != ""
			if d.Pruned && apply {
				removed, err := removeUpgradeDump(b.dir, file, d.Reason, now)
				d.Pruned = removed
				if err != nil {
					d.Error = err.Error()
					counts.Errors++
				}
			}
			if d.Pruned {
				counts.Pruned++
			} else {
				counts.Kept++
			}
			counts.Decisions = append(counts.Decisions, d)
		}
	}
	return counts, nil
}

// scanUpgradeBackups lists the backup directories under root, newest first.
func scanUpgradeBackups(root string) ([]upgradeBackup, int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, 0, fmt.Errorf("upgrade dumps: read %s: %w", root, err)
	}
	ignored := 0
	var backups []upgradeBackup
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, UpgradeBackupPrefix) {
			continue
		}
		dir := filepath.Join(root, name)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() { // a symlink is not a directory under Lstat
			ignored++
			continue
		}
		stamp := strings.TrimPrefix(name, UpgradeBackupPrefix)
		started, err := time.Parse(upgradeBackupTimeLayout, stamp)
		if err != nil || started.Format(upgradeBackupTimeLayout) != stamp {
			ignored++
			continue
		}
		b := upgradeBackup{dir: dir, started: started.UTC(), completed: readCompletedAt(dir)}
		inner, err := os.ReadDir(dir)
		if err != nil {
			ignored++
			continue
		}
		for _, f := range inner {
			if !strings.HasSuffix(f.Name(), upgradeDumpSuffix) {
				continue
			}
			fi, err := os.Lstat(filepath.Join(dir, f.Name()))
			if err != nil || !fi.Mode().IsRegular() {
				ignored++
				continue
			}
			b.dumps = append(b.dumps, f.Name())
		}
		sort.Strings(b.dumps)
		backups = append(backups, b)
	}
	sort.SliceStable(backups, func(i, j int) bool {
		if !backups[i].started.Equal(backups[j].started) {
			return backups[i].started.After(backups[j].started)
		}
		return backups[i].dir > backups[j].dir
	})
	return backups, ignored, nil
}

// readCompletedAt returns the last upgrade_completed_at in dir's STATE.txt, or
// nil when there is none, it does not parse, or the file is not a regular file.
func readCompletedAt(dir string) *time.Time {
	path := filepath.Join(dir, UpgradeStateFile)
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // path is built from a scanned directory, Lstat-checked above
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out *time.Time
	sc := bufio.NewScanner(io.LimitReader(f, maxStateFileBytes))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || k != UpgradeCompletedKey {
			continue
		}
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
			t = t.UTC()
			out = &t
		}
	}
	return out
}

// removeUpgradeDump deletes one dump, re-checking it is still a regular file,
// and records the removal in the backup's STATE.txt. removed reports whether
// the dump is gone, which stays true when only the recording failed.
func removeUpgradeDump(dir, file, reason string, now time.Time) (removed bool, err error) {
	path := filepath.Join(dir, file)
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, errors.New("not a regular file any more; left alone")
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	line := fmt.Sprintf("%s=%s reason=%s file=%s\n", UpgradePrunedKey, now.Format(time.RFC3339), reason, file)
	state := filepath.Join(dir, UpgradeStateFile)
	if fi, err := os.Lstat(state); err == nil && !fi.Mode().IsRegular() {
		return true, nil // a STATE.txt that is not a regular file is not written through
	}
	f, err := os.OpenFile(state, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600) //nolint:gosec // dir is a scanned, Lstat-checked backup directory
	if err != nil {
		return true, fmt.Errorf("dump removed, but recording it in %s failed: %w", UpgradeStateFile, err)
	}
	_, werr := f.WriteString(line)
	cerr := f.Close()
	if werr != nil {
		return true, fmt.Errorf("dump removed, but recording it in %s failed: %w", UpgradeStateFile, werr)
	}
	if cerr != nil {
		return true, fmt.Errorf("dump removed, but recording it in %s failed: %w", UpgradeStateFile, cerr)
	}
	return true, nil
}
