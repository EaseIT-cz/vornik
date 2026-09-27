package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/projectdeps"
	"vornik.io/vornik/internal/projectdeps/install"
	"vornik.io/vornik/internal/registry"
)

// `vornikctl deps` (project dependency provisioning design §8, 2026-09-25).
// The OPERATOR installs a project's declared dependencies, inside the agent
// image, on the host that runs the agents; the daemon only mounts what is
// here. vornikctl is the one thing permitted to spawn processes (the
// 2026-08-03 ruling), and the agent image's Python is the one the agents run.

var depsFrom string

var depsCmd = &cobra.Command{
	Use:   "deps",
	Short: "Install and inspect project dependency trees",
}

var depsInstallCmd = &cobra.Command{
	Use:   "install <project>",
	Short: "Install a project's declared dependencies inside its agent image",
	Long: `Install the hash-pinned lockfiles a project declares under dependencies:
into the dependency cache, running pip INSIDE the agent image its roles use, so
the installed wheels match the agents' Python. Run on the host that runs the
agent containers, as the user that runs the daemon; on a multi-node deployment,
on each node.

Only images already present on this host are used (never pulled): the image
comes from the project's swarm config. Every role must use images that agree on
one interpreter, since one tree serves them all. Source distributions are
refused (--only-binary=:all:), so no package build code ever runs, and a
lockfile that does not pin every dependency with a hash is refused.

A connected install runs pip INSIDE the image with the image's default network,
so it fetches from the package index. --from <wheelhouse> installs offline from
a directory of wheels, with no network at all; a failed connected install prints
the command that builds a wheelhouse on a connected host.

After rebuilding an agent image, run 'vornikctl deps status': the daemon checks
image references, not image IDs.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		in, err := loadDepsInput(args[0])
		if err != nil {
			return err
		}
		if depsFrom != "" {
			abs, err := filepath.Abs(depsFrom)
			if err != nil {
				return err
			}
			in.Wheelhouse = abs
		}
		return runDepsInstall(cmd.Context(), in, execRunner, cmd.OutOrStdout())
	},
}

var depsStatusCmd = &cobra.Command{
	Use:          "status [project]",
	Short:        "Show each project's dependency trees and whether they are installed",
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ins, err := loadDepsInputs(args)
		if err != nil {
			return err
		}
		return runDepsStatus(cmd.Context(), ins, execRunner, cmd.OutOrStdout())
	},
}

func init() {
	depsInstallCmd.Flags().StringVar(&depsFrom, "from", "", "install offline from this wheelhouse directory")
	depsCmd.AddCommand(depsInstallCmd, depsStatusCmd)
	rootCmd.AddCommand(depsCmd)
}

// execRunner is the real process runner: this is vornikctl, the one place a
// process may be spawned.
func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// depsInstallInput is everything an install needs, resolved from the daemon's
// config and registry before any process runs.
type depsInstallInput struct {
	ProjectID   string
	ProjectRoot string
	CacheDir    string
	Entries     []projectdeps.Entry
	// RoleImages maps each distinct role image to the roles that name it.
	RoleImages map[string][]string
	Wheelhouse string
}

func loadDepsInput(projectID string) (depsInstallInput, error) {
	ins, err := loadDepsInputs([]string{projectID})
	if err != nil {
		return depsInstallInput{}, err
	}
	return ins[0], nil
}

// loadDepsInputs reads the daemon's config and registry locally, the way
// `vornikctl retention` does: nothing the daemon answers decides what runs.
func loadDepsInputs(projectIDs []string) ([]depsInstallInput, error) {
	cfg, path, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	cacheDir := cfg.Runtime.DependencyCacheDir()
	if cacheDir == "" {
		return nil, fmt.Errorf("no dependency cache: set runtime.dependency_cache_path (or runtime.project_workspace_path)")
	}
	configsDir := resolveConfigsDir(path)
	if configsDir == "" {
		return nil, fmt.Errorf("could not locate configs/ directory (set VORNIK_CONFIGS_DIR)")
	}
	reg := registry.New()
	if err := reg.Load(configsDir); err != nil {
		return nil, fmt.Errorf("load registry from %s: %w", configsDir, err)
	}
	if len(projectIDs) == 0 {
		for _, p := range reg.ListProjects() {
			if len(p.Dependencies) > 0 {
				projectIDs = append(projectIDs, p.ID)
			}
		}
		sort.Strings(projectIDs)
	}
	out := make([]depsInstallInput, 0, len(projectIDs))
	for _, id := range projectIDs {
		project, swarm, err := reg.GetProjectWithSwarm(id)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", id, err)
		}
		in := depsInstallInput{
			ProjectID:   id,
			ProjectRoot: filepath.Join(cfg.Runtime.ProjectWorkspaceDir(), id),
			CacheDir:    cacheDir,
			Entries:     project.Dependencies,
			RoleImages:  map[string][]string{},
		}
		if swarm != nil {
			for _, role := range swarm.Roles {
				if role.Runtime.Image != "" {
					in.RoleImages[role.Runtime.Image] = append(in.RoleImages[role.Runtime.Image], role.Name)
				}
			}
		}
		out = append(out, in)
	}
	return out, nil
}

func sortedImages(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for img := range m {
		out = append(out, img)
	}
	sort.Strings(out)
	return out
}

func runDepsInstall(ctx context.Context, in depsInstallInput, run install.Runner, out io.Writer) error {
	if len(in.Entries) == 0 {
		_, _ = fmt.Fprintf(out, "project %s declares no dependencies; nothing to install\n", in.ProjectID)
		return nil
	}
	if err := install.CheckOwner(in.CacheDir); err != nil {
		return err
	}
	images := sortedImages(in.RoleImages)
	if len(images) == 0 {
		return fmt.Errorf("project %s: no role declares an image to install for", in.ProjectID)
	}
	// Every image is confirmed LOCAL, and shown, before anything runs in one:
	// the references come from a control-plane-editable config.
	ids := map[string]string{}
	for _, img := range images {
		id, err := install.ImageID(ctx, run, img)
		if err != nil {
			return err
		}
		ids[img] = id
		_, _ = fmt.Fprintf(out, "image %s  %s  (roles: %s)\n", img, id, strings.Join(in.RoleImages[img], ", "))
	}
	interp, err := install.ChooseInterpreter(ctx, run, images)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "interpreter %s\n", interp)

	store := projectdeps.NewStore(in.CacheDir)
	meta := projectdeps.MarkerMeta{Images: images, ImageIDs: ids, Interpreter: interp}
	for _, p := range projectdeps.NewResolver(store, "").Plan(in.ProjectRoot, in.Entries) {
		if p.Problem != nil {
			return fmt.Errorf("%s: %w", p.Entry.Lockfile, p.Problem)
		}
		if p.Entry.Ecosystem != projectdeps.EcosystemPip {
			return fmt.Errorf("%s: %s is not yet installable — slice 1 provisions pip only", p.Entry.Lockfile, p.Entry.Ecosystem)
		}
		if err := install.ConfinedLockfile(in.ProjectRoot, p.LockfilePath); err != nil {
			return err
		}
		if p.Materialised {
			if err := checkInstalledFor(store, p, images); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "already installed  %s  %s\n", p.Entry.Lockfile, p.Key)
			continue
		}
		fetch := install.PipFetcher(run, images[0], p.LockfilePath, in.Wheelhouse)
		if _, err := install.Materialise(ctx, store, p.Key, fetch, meta); err != nil {
			if in.Wheelhouse == "" {
				_, _ = fmt.Fprintf(out, "an air-gapped host can install from a wheelhouse built on a connected one:\n  %s\nthen: vornikctl deps install %s --from ./wheelhouse\n",
					install.WheelhouseCommand(images[0], p.LockfilePath), in.ProjectID)
			}
			return err
		}
		_, _ = fmt.Fprintf(out, "installed  %s  %s\n", p.Entry.Lockfile, p.Key)
	}
	return nil
}

// checkInstalledFor refuses an existing tree that was installed for images
// other than the project's current ones. The key does not include the image,
// so the tree is never replaced silently: the operator removes it knowingly.
func checkInstalledFor(store *projectdeps.Store, p projectdeps.Plan, images []string) error {
	meta, err := store.ReadMarker(p.Key)
	if err != nil {
		return err
	}
	for _, img := range images {
		if !meta.ListsImage(img) {
			return fmt.Errorf("%s is installed for %v, not for %s: remove %s and re-run to install it for the current images",
				p.Key, meta.Images, img, store.Path(p.Key))
		}
	}
	return nil
}

func runDepsStatus(ctx context.Context, ins []depsInstallInput, run install.Runner, out io.Writer) error {
	if len(ins) == 0 {
		_, _ = fmt.Fprintln(out, "no project declares dependencies")
		return nil
	}
	for _, in := range ins {
		store := projectdeps.NewStore(in.CacheDir)
		for _, p := range projectdeps.NewResolver(store, "").Plan(in.ProjectRoot, in.Entries) {
			switch {
			case p.Problem != nil:
				_, _ = fmt.Fprintf(out, "%s  %s  BROKEN: %v\n", in.ProjectID, p.Entry.Lockfile, p.Problem)
			case !p.Materialised:
				_, _ = fmt.Fprintf(out, "%s  %s  not installed (%s): vornikctl deps install %s\n", in.ProjectID, p.Entry.Lockfile, p.Key, in.ProjectID)
			default:
				meta, err := store.ReadMarker(p.Key)
				if err != nil {
					_, _ = fmt.Fprintf(out, "%s  %s  installed, marker unreadable: %v\n", in.ProjectID, p.Entry.Lockfile, err)
					continue
				}
				_, _ = fmt.Fprintf(out, "%s  %s  installed (%s) for %s\n", in.ProjectID, p.Entry.Lockfile, p.Key, meta.Interpreter)
				for _, img := range meta.Images {
					note := ""
					if now, err := install.ImageID(ctx, run, img); err != nil {
						note = "  (not present on this host)"
					} else if now != meta.ImageIDs[img] {
						note = fmt.Sprintf("  (image changed since install: now %s; re-check it runs the same Python)", now)
					}
					_, _ = fmt.Fprintf(out, "    %s  %s%s\n", img, meta.ImageIDs[img], note)
				}
			}
		}
	}
	return nil
}
