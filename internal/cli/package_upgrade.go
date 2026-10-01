package cli

// `vornikctl package upgrade` — package design §8. Provenance keeps each
// contributed file's hash at install, not its content, so an upgrade decides
// per file and never merges lines. Any edited or deleted contribution refuses
// the whole upgrade. Files are written first, then the package's provenance
// is swapped in one transaction.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/agentpackage"
	"vornik.io/vornik/internal/persistence"
)

var (
	packageUpgradeDryRun bool

	packageUpgradeCmd = &cobra.Command{
		Use:   "upgrade <dir-or-tarball>",
		Short: "Upgrade an installed package to the version in an archive",
		Long: `Replace an installed package's contributions with the version in an
archive, deciding per file:

  unedited                  replaced with the new version (or kept if identical)
  already the new version   adopted
  dropped by the new version removed if unedited
  new in this version       added, with install's conflict checks

Provenance records each file's hash at install, not its content, so an
upgrade cannot merge: a contribution you EDITED or DELETED refuses the whole
upgrade, naming every file. Restore it (or copy your tuning aside), upgrade,
then reapply.

The daemon picks the new config up on its next reload.`,
		Args: cobra.ExactArgs(1),
		RunE: runPackageUpgrade,
	}
)

func init() {
	packageUpgradeCmd.Flags().BoolVar(&packageUpgradeDryRun, "dry-run", false, "Print the plan without touching disk or the provenance store")
	packageCmd.AddCommand(packageUpgradeCmd)
}

// upgradeRepo is the provenance surface an upgrade needs.
type upgradeRepo interface {
	ContributionsByPackage(ctx context.Context, pkg string) ([]persistence.PackageContribution, error)
	ContributionOwner(ctx context.Context, kind, rowID string) (string, bool, error)
	ReplaceContributions(ctx context.Context, pkg string, rows []persistence.PackageContribution) error
}

// Seams for the apply-layer tests: every file write goes through
// upgradeWriteFile, and upgradeBeforeWrite runs before each file step.
var (
	upgradeWriteFile   = writeUpgradeFile
	upgradeBeforeWrite = func(string) {}
)

func runPackageUpgrade(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()
	pc, err := openPackageContext(ctx)
	if err != nil {
		return err
	}
	defer pc.close()
	src, err := agentpackage.Open(args[0])
	if err != nil {
		return err
	}
	defer src.Close()
	return upgradePackage(ctx, os.Stdout, pc.configsDir, pc.repo, src, packageUpgradeDryRun)
}

// upgradePackage plans, prints and applies one upgrade.
func upgradePackage(ctx context.Context, out io.Writer, configsDir string, repo upgradeRepo, src *agentpackage.Source, dryRun bool) error {
	m := src.Manifest
	stored, err := repo.ContributionsByPackage(ctx, m.Package)
	if err != nil {
		return err
	}
	installed := make([]agentpackage.Contribution, 0, len(stored))
	fromVersion := ""
	for _, s := range stored {
		fromVersion = s.PackageVersion
		installed = append(installed, agentpackage.Contribution{
			Package: s.Package, Kind: agentpackage.Kind(s.Kind), RowID: s.RowID,
			Path: s.Path, ContentHashAtInstall: s.ContentHashAtInstall,
		})
	}
	plan, err := agentpackage.PlanUpgrade(m, fromVersion, installed, upgradeEnvironment(ctx, configsDir, repo, src))
	if err != nil {
		return err
	}
	printUpgradePlan(out, configsDir, plan)
	if plan.IsNoop() {
		_, _ = fmt.Fprintln(out, "\nNothing to do: this version is already installed and unchanged.")
		return nil
	}
	if dryRun {
		_, _ = fmt.Fprintln(out, "\n--dry-run: nothing was written.")
		return nil
	}
	if err := applyUpgrade(configsDir, plan); err != nil {
		return err
	}
	rows := make([]persistence.PackageContribution, 0)
	now := time.Now().UTC()
	for _, c := range plan.Contributions() {
		rows = append(rows, persistence.PackageContribution{
			Kind: string(c.Kind), RowID: c.RowID, Package: c.Package, PackageVersion: m.Version,
			Path: c.Path, ContentHashAtInstall: c.ContentHashAtInstall, InstalledAt: now,
		})
	}
	if err := repo.ReplaceContributions(ctx, m.Package, rows); err != nil {
		return fmt.Errorf("the files are now %s %s, but recording its provenance failed (%w). "+
			"Until it is recorded, `package list` shows the replaced files as [edited] and uninstall will refuse. "+
			"re-run `vornikctl package upgrade` with the same archive to finish: it adopts the files already written",
			m.Package, m.Version, err)
	}
	_, _ = fmt.Fprintf(out, "\nUpgraded %s %s → %s. Run `vornikctl config reload` (or wait for the daemon's next reload) to activate it.\n",
		m.Package, plan.FromVersion, m.Version)
	return nil
}

func upgradeEnvironment(ctx context.Context, configsDir string, repo upgradeRepo, src *agentpackage.Source) agentpackage.UpgradeEnvironment {
	return agentpackage.UpgradeEnvironment{
		Environment: agentpackage.Environment{
			ReadPayload: src.ReadPayload,
			DeployedExists: func(rel string) bool {
				_, statErr := os.Stat(filepath.Join(configsDir, rel))
				return statErr == nil
			},
			ClaimedBy: func(kind agentpackage.Kind, rowID string) (string, bool) {
				owner, ok, ownerErr := repo.ContributionOwner(ctx, string(kind), rowID)
				if ownerErr != nil {
					// Fail closed, as install does, and say why: the refusal
					// that follows names an "unknown" owner, and the operator
					// must not go looking for another package.
					fmt.Fprintf(os.Stderr, "warning: provenance lookup for %s %q failed (%v); treating it as claimed\n", kind, rowID, ownerErr)
					return "unknown (provenance lookup failed)", true
				}
				return owner, ok
			},
		},
		ReadDeployed: func(rel string) ([]byte, bool) {
			b, readErr := os.ReadFile(filepath.Join(configsDir, rel))
			return b, readErr == nil
		},
	}
}

func printUpgradePlan(out io.Writer, configsDir string, plan *agentpackage.UpgradePlan) {
	_, _ = fmt.Fprintf(out, "Package %s %s → %s:\n", plan.Manifest.Package, plan.FromVersion, plan.Manifest.Version)
	line := func(verb string, kind agentpackage.Kind, rowID, rel string) {
		_, _ = fmt.Fprintf(out, "  %-8s %-8s %-28s %s\n", verb, kind, rowID, filepath.Join(configsDir, rel))
	}
	for _, r := range plan.Replace {
		line("replace", r.Kind, r.RowID, r.TargetPath)
	}
	for _, it := range plan.Add {
		line("add", it.Kind, it.RowID, it.TargetPath)
	}
	for _, c := range plan.Remove {
		line("remove", c.Kind, c.RowID, c.Path)
	}
	for _, it := range plan.Adopt {
		line("adopt", it.Kind, it.RowID, it.TargetPath)
	}
	for _, it := range plan.Reclaim {
		line("reclaim", it.Kind, it.RowID, it.TargetPath)
	}
	for _, c := range plan.Drop {
		line("drop", c.Kind, c.RowID, c.Path)
	}
	for _, it := range plan.Keep {
		line("keep", it.Kind, it.RowID, it.TargetPath)
	}
}

// undoStep restores one file to what it was before the upgrade touched it:
// prior==nil means it did not exist.
type undoStep struct {
	path  string
	prior []byte
}

// applyUpgrade writes replacements, then additions, then removals, each
// re-checked at write time (design §8). On any failure it restores every
// file it changed and names those it could not restore.
func applyUpgrade(configsDir string, plan *agentpackage.UpgradePlan) error {
	var undo []undoStep
	fail := func(cause error) error { return restoreUpgrade(undo, cause) }

	for _, r := range plan.Replace {
		path := filepath.Join(configsDir, r.TargetPath)
		upgradeBeforeWrite(r.TargetPath)
		prior, err := os.ReadFile(path)
		if err != nil || agentpackage.ContentHash(prior) != r.CurrentHash {
			return fail(fmt.Errorf("%s changed after the upgrade was planned; nothing was overwritten", r.TargetPath))
		}
		if err := upgradeWriteFile(path, r.Bytes, false); err != nil {
			return fail(fmt.Errorf("write %s: %w", r.TargetPath, err))
		}
		undo = append(undo, undoStep{path: path, prior: prior})
	}
	for _, it := range plan.Add {
		path := filepath.Join(configsDir, it.TargetPath)
		upgradeBeforeWrite(it.TargetPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fail(fmt.Errorf("create %s: %w", filepath.Dir(path), err))
		}
		if err := upgradeWriteFile(path, it.Bytes, true); err != nil {
			return fail(fmt.Errorf("add %s: %w", it.TargetPath, err))
		}
		undo = append(undo, undoStep{path: path})
	}
	for _, c := range plan.Remove {
		path := filepath.Join(configsDir, c.Path)
		upgradeBeforeWrite(c.Path)
		prior, err := os.ReadFile(path)
		if err != nil || agentpackage.ContentHash(prior) != c.ContentHashAtInstall {
			return fail(fmt.Errorf("%s changed after the upgrade was planned; nothing was removed", c.Path))
		}
		if err := os.Remove(path); err != nil {
			return fail(fmt.Errorf("remove %s: %w", c.Path, err))
		}
		undo = append(undo, undoStep{path: path, prior: prior})
	}
	return nil
}

// restoreUpgrade undoes the steps in reverse, best-effort, and names any file
// it could not restore.
func restoreUpgrade(undo []undoStep, cause error) error {
	var stuck []string
	for i := len(undo) - 1; i >= 0; i-- {
		u := undo[i]
		var err error
		if u.prior == nil {
			err = os.Remove(u.path)
			if os.IsNotExist(err) {
				err = nil
			}
		} else {
			err = upgradeWriteFile(u.path, u.prior, false)
		}
		if err != nil {
			stuck = append(stuck, u.path)
		}
	}
	if len(stuck) > 0 {
		return fmt.Errorf("%w (and these files could not be restored — fix them by hand: %v)", cause, stuck)
	}
	return fmt.Errorf("%w; every file the upgrade had changed was restored", cause)
}

// writeUpgradeFile writes a file. exclusive creates it with O_EXCL, so a path
// that appeared after planning stops the upgrade instead of being overwritten.
// A non-exclusive write goes to a temporary file in the same directory and is
// renamed over the target, so a crash mid-write never leaves a half-written
// workflow behind.
func writeUpgradeFile(path string, b []byte, exclusive bool) error {
	if exclusive {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(b); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return err
		}
		return f.Close()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vornik-upgrade-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
