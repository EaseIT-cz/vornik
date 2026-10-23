package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/retention"
)

// GitHub #394: the upgrade pg_dumps vornik-update.sh writes were never pruned.
// `vornikctl retention upgrade-dumps` is the one entry point the script, the
// operator and (through the same function) the daemon use. This drives the
// command on a directory laid out exactly as the script writes it, with keep
// and grace read from the daemon config.
//
// Found at: review (Art 17 copies inventory). Observed at: integration (dump
// files persisting past retention).
//
// see LLD § https://docs.vornik.io §8.2.3
func TestRetentionUpgradeDumpsCLI_AppliesConfiguredPolicy(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	mk := func(started, completed time.Time) string {
		dir := filepath.Join(root, retention.UpgradeBackupPrefix+started.Format("20060102T150405Z"))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		state := "pre_upgrade_commit=aaaaaaa\n"
		if !completed.IsZero() {
			state += retention.UpgradeCompletedKey + "=" + completed.Format(time.RFC3339) + "\n"
		}
		for name, body := range map[string]string{
			"vornik-vornik-" + started.Format("20060102") + ".dump": "PGDMP",
			"vornik.prev": "bin", "STATE.txt": state,
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	// Completed 20 days ago: inside a configured 30-day grace, outside the
	// 14-day default — so the configured value is what keeps it.
	inGrace := mk(now.Add(-20*24*time.Hour), now.Add(-20*24*time.Hour))
	// Completed 40 days ago: past the configured grace.
	pastGrace := mk(now.Add(-40*24*time.Hour), now.Add(-40*24*time.Hour))

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  address: \":8080\"\napi:\n  auth_enabled: false\n"+
		"retention:\n  upgrade_dump_keep: 5\n  upgrade_dump_grace_days: 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VORNIK_CONFIG", cfgPath)

	var out bytes.Buffer
	cmd := retentionUpgradeDumpsCmd
	cmd.SetOut(&out)
	upgradeDumpsDir, upgradeDumpsApply, upgradeDumpsJSON = root, true, true
	t.Cleanup(func() { upgradeDumpsDir, upgradeDumpsApply, upgradeDumpsJSON = "", false, false; cmd.SetOut(nil) })
	if err := runRetentionUpgradeDumps(cmd, nil); err != nil {
		t.Fatalf("vornikctl retention upgrade-dumps --apply: %v\n%s", err, out.String())
	}
	var got struct {
		Applied   bool `json:"applied"`
		Keep      int  `json:"keep"`
		GraceDays int  `json:"grace_days"`
		Examined  int  `json:"examined"`
		Pruned    int  `json:"pruned"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if !got.Applied || got.Keep != 5 || got.GraceDays != 30 || got.Examined != 2 || got.Pruned != 1 {
		t.Errorf("summary = %+v, want applied, keep 5, grace 30, 2 examined, 1 pruned", got)
	}
	if m, _ := filepath.Glob(filepath.Join(inGrace, "*.dump")); len(m) != 1 {
		t.Errorf("a dump 20 days past its upgrade must be kept under a configured 30-day grace")
	}
	if m, _ := filepath.Glob(filepath.Join(pastGrace, "*.dump")); len(m) != 0 {
		t.Errorf("a dump 40 days past its upgrade must be pruned under a 30-day grace")
	}
}

// The script and the pruner share one layout contract (design §8.2.3). The
// completion key the pruner reads must be the one vornik-update.sh writes, and
// the script must call this verb on the root it writes to.
func TestRetentionUpgradeDumps_ScriptSharesTheContract(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "deployments", "podman", "vornik-update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		`BK="$HOME/` + retention.UpgradeBackupPrefix,
		retention.UpgradeCompletedKey + "=",
		`retention upgrade-dumps --dir "$HOME" --apply`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("vornik-update.sh does not contain %q: the script and the pruner have drifted apart", want)
		}
	}
}
