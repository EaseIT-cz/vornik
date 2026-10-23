package retention

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// GitHub #394: vornik-update.sh wrote a full pg_dump of the database into
// $HOME/vornik-upgrade-backup-<UTC>/ on every upgrade and nothing ever removed
// one, so every Art 17 erasure made after a dump was undone in it.
//
// Found at: review (the Art 17 copies inventory, design 2026-07-29 §15.11.2).
// Observed at: integration — dump files persisting past any retention. These
// tests run the pruner on a real directory tree laid out as the script writes
// it.
//
// see LLD § https://docs.vornik.io §8.2.3

var dumpNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// backup lays out one backup directory the way vornik-update.sh step 1 does.
// completed is the upgrade_completed_at stamp; zero writes none.
func backup(t *testing.T, root string, started time.Time, completed time.Time) string {
	t.Helper()
	dir := filepath.Join(root, UpgradeBackupPrefix+started.UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("vornik-vornik-"+started.UTC().Format("20060102")+".dump", "PGDMP a full copy of the database")
	write("vornik.prev", "binary")
	write("vornikctl.prev", "binary")
	write("config.yaml", "listen: x\n")
	state := "pre_upgrade_commit=aaaaaaa\npre_upgrade_version=2026.10.5\ntarget_ref=2026.10.6\n"
	if !completed.IsZero() {
		state += UpgradeCompletedKey + "=" + completed.UTC().Format(time.RFC3339) + "\n"
	}
	write("STATE.txt", state)
	return dir
}

func dumpsIn(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.dump"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func TestPruneUpgradeDumps_KeepNewestN(t *testing.T) {
	root := t.TempDir()
	var dirs []string
	for i := 4; i >= 1; i-- { // oldest first; all completed a day after start, within grace
		started := dumpNow.Add(-days(i) - time.Hour)
		dirs = append(dirs, backup(t, root, started, started.Add(time.Hour)))
	}
	c, err := PruneUpgradeDumps(UpgradeDumpPolicy{Root: root, Keep: 2, GraceDays: 14}, dumpNow, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Examined != 4 || c.Pruned != 2 || c.Kept != 2 {
		t.Fatalf("want 4 examined, 2 pruned, 2 kept; got %+v", c)
	}
	for i, d := range dirs {
		got := len(dumpsIn(t, d))
		want := 0
		if i >= 2 {
			want = 1
		}
		if got != want {
			t.Errorf("%s: %d dump(s), want %d", filepath.Base(d), got, want)
		}
	}
	for _, d := range c.Decisions {
		if d.Pruned && d.Reason != PruneReasonKeep {
			t.Errorf("%s pruned for %q, want %q", d.File, d.Reason, PruneReasonKeep)
		}
	}
}

func TestPruneUpgradeDumps_GraceAfterSuccessfulUpgrade(t *testing.T) {
	root := t.TempDir()
	old := backup(t, root, dumpNow.Add(-days(20)), dumpNow.Add(-days(15)))   // completed 15 days ago
	young := backup(t, root, dumpNow.Add(-days(13)), dumpNow.Add(-days(13))) // completed 13 days ago
	c, err := PruneUpgradeDumps(UpgradeDumpPolicy{Root: root, Keep: 5, GraceDays: 14}, dumpNow, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dumpsIn(t, old)) != 0 {
		t.Errorf("a dump 15 days past its successful upgrade must be pruned (grace 14)")
	}
	if len(dumpsIn(t, young)) != 1 {
		t.Errorf("a dump 13 days past its successful upgrade must be kept (grace 14)")
	}
	if c.Pruned != 1 {
		t.Fatalf("want one prune; got %+v", c)
	}
	for _, d := range c.Decisions {
		if d.Pruned && d.Reason != PruneReasonGrace {
			t.Errorf("%s pruned for %q, want %q", d.File, d.Reason, PruneReasonGrace)
		}
	}
}

// The newest dump of a healthy install also goes, 14 days after its upgrade.
func TestPruneUpgradeDumps_NewestStampedDumpAgesOut(t *testing.T) {
	root := t.TempDir()
	d := backup(t, root, dumpNow.Add(-days(30)), dumpNow.Add(-days(30)))
	if _, err := PruneUpgradeDumps(UpgradeDumpPolicy{Root: root}, dumpNow, true); err != nil {
		t.Fatal(err)
	}
	if len(dumpsIn(t, d)) != 0 {
		t.Errorf("the only dump, 30 days past its successful upgrade, must be pruned on the defaults")
	}
}

// An unstamped dump (a failed run, or a backup from before #394) is superseded
// when a newer backup exists; the newest unstamped dump has no superseded time
// and is the rollback point of the upgrade that just failed.
func TestPruneUpgradeDumps_UnstampedDumps(t *testing.T) {
	root := t.TempDir()
	legacy := backup(t, root, dumpNow.Add(-days(60)), time.Time{})       // superseded 20 days ago
	recentLegacy := backup(t, root, dumpNow.Add(-days(20)), time.Time{}) // superseded 3 days ago
	failedNewest := backup(t, root, dumpNow.Add(-days(3)), time.Time{})  // nothing newer
	c, err := PruneUpgradeDumps(UpgradeDumpPolicy{Root: root, Keep: 5, GraceDays: 14}, dumpNow, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dumpsIn(t, legacy)) != 0 {
		t.Errorf("an unstamped dump superseded 20 days ago must be pruned")
	}
	if len(dumpsIn(t, recentLegacy)) != 1 {
		t.Errorf("an unstamped dump superseded 3 days ago must be kept")
	}
	if len(dumpsIn(t, failedNewest)) != 1 {
		t.Errorf("the newest unstamped dump must be kept: it may be the only way back from a failed upgrade")
	}
	if c.Examined != 3 || c.Pruned != 1 || c.Kept != 2 {
		t.Errorf("counts: %+v", c)
	}

	// Even a very old newest unstamped dump stays by the grace rule.
	root2 := t.TempDir()
	only := backup(t, root2, dumpNow.Add(-days(400)), time.Time{})
	if _, err := PruneUpgradeDumps(UpgradeDumpPolicy{Root: root2}, dumpNow, true); err != nil {
		t.Fatal(err)
	}
	if len(dumpsIn(t, only)) != 1 {
		t.Errorf("the newest unstamped dump must survive the grace rule whatever its age")
	}
}

func TestPruneUpgradeDumps_PreviewDeletesNothing(t *testing.T) {
	root := t.TempDir()
	d := backup(t, root, dumpNow.Add(-days(30)), dumpNow.Add(-days(30)))
	c, err := PruneUpgradeDumps(UpgradeDumpPolicy{Root: root}, dumpNow, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pruned != 1 {
		t.Errorf("preview must count the due dump; got %+v", c)
	}
	if len(dumpsIn(t, d)) != 1 {
		t.Errorf("preview deleted a dump")
	}
	state, _ := os.ReadFile(filepath.Join(d, "STATE.txt"))
	if strings.Contains(string(state), UpgradePrunedKey) {
		t.Errorf("preview wrote to STATE.txt")
	}
}

func TestPruneUpgradeDumps_ApplyRecordsAndKeepsTheRest(t *testing.T) {
	root := t.TempDir()
	d := backup(t, root, dumpNow.Add(-days(30)), dumpNow.Add(-days(30)))
	if _, err := PruneUpgradeDumps(UpgradeDumpPolicy{Root: root}, dumpNow, true); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"vornik.prev", "vornikctl.prev", "config.yaml", "STATE.txt"} {
		if _, err := os.Lstat(filepath.Join(d, keep)); err != nil {
			t.Errorf("%s must stay: %v", keep, err)
		}
	}
	state, err := os.ReadFile(filepath.Join(d, "STATE.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), UpgradePrunedKey+"=2026-10-16T12:00:00Z reason=grace file=vornik-vornik-") {
		t.Errorf("STATE.txt must record the prune; got:\n%s", state)
	}
	if !strings.Contains(string(state), "pre_upgrade_commit=aaaaaaa") {
		t.Errorf("STATE.txt lost its earlier keys:\n%s", state)
	}
}

func TestPruneUpgradeDumps_IgnoresWhatIsNotTheLayout(t *testing.T) {
	root := t.TempDir()
	// A real dump elsewhere, reachable only through symlinks.
	elsewhere := t.TempDir()
	target := filepath.Join(elsewhere, "precious.dump")
	if err := os.WriteFile(target, []byte("PGDMP"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 1. A backup dir whose dump is a symlink.
	d := backup(t, root, dumpNow.Add(-days(30)), dumpNow.Add(-days(30)))
	for _, m := range dumpsIn(t, d) {
		_ = os.Remove(m)
	}
	if err := os.Symlink(target, filepath.Join(d, "linked.dump")); err != nil {
		t.Fatal(err)
	}
	// 2. A symlinked backup dir.
	if err := os.Symlink(elsewhere, filepath.Join(root, UpgradeBackupPrefix+"20200101T000000Z")); err != nil {
		t.Fatal(err)
	}
	// 3. A directory whose name does not parse.
	bad := filepath.Join(root, UpgradeBackupPrefix+"yesterday")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "x.dump"), []byte("PGDMP"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 4. An unrelated directory.
	if err := os.MkdirAll(filepath.Join(root, "Documents"), 0o755); err != nil {
		t.Fatal(err)
	}

	c, err := PruneUpgradeDumps(UpgradeDumpPolicy{Root: root}, dumpNow, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("a dump reached through a symlink was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bad, "x.dump")); err != nil {
		t.Fatalf("a dump in a directory whose name does not parse was deleted: %v", err)
	}
	if c.Examined != 0 || c.Pruned != 0 {
		t.Errorf("nothing in the layout was examined; got %+v", c)
	}
	if c.Ignored < 3 {
		t.Errorf("the symlinked dump, the symlinked dir and the unparsable dir must be counted as ignored; got %+v", c)
	}
}

func TestPruneUpgradeDumps_UnreadableRootIsAnError(t *testing.T) {
	_, err := PruneUpgradeDumps(UpgradeDumpPolicy{Root: filepath.Join(t.TempDir(), "absent")}, dumpNow, false)
	if err == nil {
		t.Fatal("a root that cannot be read must be an error, not zero examined")
	}
}

func TestUpgradeDumpPolicy_Defaults(t *testing.T) {
	p := UpgradeDumpPolicy{Keep: -1, GraceDays: 0}.withDefaults()
	if p.Keep != DefaultUpgradeDumpKeep || p.GraceDays != DefaultUpgradeDumpGraceDays {
		t.Fatalf("got %+v", p)
	}
	if DefaultUpgradeDumpKeep != 2 || DefaultUpgradeDumpGraceDays != 14 {
		t.Fatalf("the design's defaults are keep 2, grace 14 days")
	}
}
