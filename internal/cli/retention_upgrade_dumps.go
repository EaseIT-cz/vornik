package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/retention"
)

var (
	upgradeDumpsDir   string
	upgradeDumpsApply bool
	upgradeDumpsJSON  bool
)

// retentionUpgradeDumpsCmd prunes the database dumps vornik-update.sh leaves in
// vornik-upgrade-backup-* directories (GitHub #394). It is the one entry point
// the script (after a successful upgrade), the operator and — through the same
// retention.PruneUpgradeDumps — the daemon sweeper share. It needs no database.
//
// see LLD § https://docs.vornik.io §8.2.3
var retentionUpgradeDumpsCmd = &cobra.Command{
	Use:   "upgrade-dumps",
	Short: "Preview or prune the database dumps left by vornik-update.sh",
	Long: `Each run of vornik-update.sh writes a full database dump into
<dir>/vornik-upgrade-backup-<UTC>/. A dump is a complete copy of the
database, so an erasure made after it is undone in it. This command keeps
at most retention.upgrade_dump_keep dumps (default 2, newest first) and
removes a dump retention.upgrade_dump_grace_days (default 14) after the
upgrade that wrote it succeeded — or, for a run that did not succeed,
after a newer upgrade ran. The newest dump of a failed upgrade is kept.

Only the .dump file is removed; the binaries, config.yaml and STATE.txt
stay, and the removal is recorded in STATE.txt. Preview by default; add
--apply to delete. The daemon's retention sweeper applies the same rules
when retention.enabled is on.

Examples:
  vornikctl retention upgrade-dumps                 # preview, in your home directory
  vornikctl retention upgrade-dumps --apply
  vornikctl retention upgrade-dumps --dir /srv/vornik-backups --json
`,
	RunE: runRetentionUpgradeDumps,
}

func init() {
	retentionUpgradeDumpsCmd.Flags().StringVar(&upgradeDumpsDir, "dir", "", "directory holding the vornik-upgrade-backup-* directories (default: retention.upgrade_dump_dir, else your home)")
	retentionUpgradeDumpsCmd.Flags().BoolVar(&upgradeDumpsApply, "apply", false, "actually delete the dumps past the policy (default: preview only)")
	retentionUpgradeDumpsCmd.Flags().BoolVar(&upgradeDumpsJSON, "json", false, "emit machine-readable JSON")
	retentionCmd.AddCommand(retentionUpgradeDumpsCmd)
}

// upgradeDumpPolicyFromConfig is the policy the daemon config sets, or the
// compiled defaults when no config loads (the command must still work on a
// host whose config is broken: it is run at the end of an upgrade).
func upgradeDumpPolicyFromConfig() (retention.UpgradeDumpPolicy, error) {
	cfg, _, err := config.Load()
	if err != nil || cfg == nil {
		return retention.UpgradeDumpPolicy{}, err
	}
	_, p := retention.GlobalFromConfig(cfg.Retention)
	return p, nil
}

func runRetentionUpgradeDumps(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	policy, cfgErr := upgradeDumpPolicyFromConfig()
	if cfgErr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: config not loaded (%v); using the default policy\n", cfgErr)
	}
	if upgradeDumpsDir != "" {
		policy.Root = upgradeDumpsDir
	}
	counts, err := retention.PruneUpgradeDumps(policy, time.Now(), upgradeDumpsApply)
	if err != nil {
		return err
	}
	if upgradeDumpsJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Applied bool `json:"applied"`
			retention.UpgradeDumpCounts
		}{upgradeDumpsApply, counts}); err != nil {
			return err
		}
	} else {
		verb := "would prune"
		if upgradeDumpsApply {
			verb = "pruned"
		}
		_, _ = fmt.Fprintf(out, "upgrade dumps in %s: %d examined, %d %s, %d kept, %d ignored (keep %d, grace %d days)\n",
			counts.Root, counts.Examined, counts.Pruned, verb, counts.Kept, counts.Ignored, counts.Keep, counts.GraceDays)
		for _, d := range counts.Decisions {
			state := "kept"
			if d.Pruned {
				state = verb + " (" + d.Reason + ")"
			}
			if d.Error != "" {
				state += ": " + d.Error
			}
			_, _ = fmt.Fprintf(out, "  %s/%s  %s\n", d.Dir, d.File, state)
		}
		if !upgradeDumpsApply && counts.Pruned > 0 {
			_, _ = fmt.Fprintln(out, "preview only — use --apply to delete.")
		}
	}
	if counts.Errors > 0 {
		return fmt.Errorf("%d dump(s) could not be pruned", counts.Errors)
	}
	return nil
}
