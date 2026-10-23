package hostdoctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/retention"
)

// GitHub #394: upgrade pg_dumps were never pruned and nothing told the
// operator they were there. The upgrade_dumps host check reports them against
// the policy, with the denominator (design §8.2.3).
func TestCheckUpgradeDumps(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mk := func(root string, started, completed time.Time) {
		dir := filepath.Join(root, retention.UpgradeBackupPrefix+started.Format("20060102T150405Z"))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		state := retention.UpgradeCompletedKey + "=" + completed.Format(time.RFC3339) + "\n"
		_ = os.WriteFile(filepath.Join(dir, "STATE.txt"), []byte(state), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "vornik-vornik-"+started.Format("20060102")+".dump"), []byte("PGDMP"), 0o644)
	}
	check := func(root string) Check {
		h := New("", "", "").WithUpgradeDumps(retention.UpgradeDumpPolicy{Root: root})
		h.nowFunc = func() time.Time { return now }
		return h.checkUpgradeDumps(context.Background())
	}

	t.Run("none found is OK and says 0", func(t *testing.T) {
		c := check(t.TempDir())
		if c.Name != "upgrade_dumps" || c.Status != "OK" || !strings.Contains(c.Message, "0 dump(s) examined") {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("within policy is OK with the denominator", func(t *testing.T) {
		root := t.TempDir()
		mk(root, now.Add(-48*time.Hour), now.Add(-47*time.Hour))
		c := check(root)
		if c.Status != "OK" || !strings.Contains(c.Message, "1 dump(s) examined") || !strings.Contains(c.Message, "keep 2, grace 14 days") {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("past policy warns and names each dump", func(t *testing.T) {
		root := t.TempDir()
		mk(root, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour))
		mk(root, now.Add(-48*time.Hour), now.Add(-47*time.Hour))
		c := check(root)
		if c.Status != "WARNING" || !strings.Contains(c.Message, "1 of 2 dump(s)") ||
			!strings.Contains(c.Message, "vornikctl retention upgrade-dumps --apply") || len(c.Items) != 1 ||
			!strings.Contains(c.Items[0], "grace") {
			t.Fatalf("got %+v", c)
		}
		// The check never deletes anything.
		if m, _ := filepath.Glob(filepath.Join(root, "*", "*.dump")); len(m) != 2 {
			t.Fatalf("the doctor deleted a dump: %v", m)
		}
	})
	t.Run("unreadable root is SKIPPED with the error", func(t *testing.T) {
		c := check(filepath.Join(t.TempDir(), "absent"))
		if c.Status != "SKIPPED" || !strings.Contains(c.Message, "absent") {
			t.Fatalf("got %+v", c)
		}
	})
}
