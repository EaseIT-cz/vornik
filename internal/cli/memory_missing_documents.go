package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/memory"
)

// Missing document cleanup (memory rollback x supersession design, amendment
// A.8, GitHub #701). A path-identity document whose path is no longer in the
// operator's checkout is retired. Like A.5's verb this is a client verb: only
// the checkout knows which files exist. No decision rests on a base name.

const (
	missingPresent = "present"
	missingMissing = "missing"
)

type missingPlanRow struct {
	Path     string
	Chunks   int
	Decision string
	Survivor string   // the upload the plan saw live; apply retires only while it is unchanged
	MovedTo  []string // information only: tracked files with the same base name whose own path is live
}

// planMissingDocumentsWith marks each path document present or missing. Paths
// are compared case-sensitively after path.Clean on both sides.
func planMissingDocumentsWith(docs []memory.LegacyDocument, tracked []string, survivor func(string) (string, error)) ([]missingPlanRow, error) {
	inTree := map[string]bool{}
	byBase := map[string][]string{}
	for _, f := range tracked {
		c := path.Clean(f)
		inTree[c] = true
		byBase[path.Base(c)] = append(byBase[path.Base(c)], c)
	}
	rows := make([]missingPlanRow, 0, len(docs))
	for _, d := range docs {
		row := missingPlanRow{Path: d.Name, Chunks: d.Chunks, Decision: missingPresent}
		if !inTree[path.Clean(d.Name)] {
			row.Decision = missingMissing
			s, err := survivor(d.Name)
			if err != nil {
				return nil, fmt.Errorf("look up %s: %w", d.Name, err)
			}
			row.Survivor = s
			for _, c := range byBase[path.Base(path.Clean(d.Name))] {
				live, err := survivor(c)
				if err != nil {
					return nil, fmt.Errorf("look up %s: %w", c, err)
				}
				if live != "" {
					row.MovedTo = append(row.MovedTo, c)
				}
			}
			sort.Strings(row.MovedTo)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// renderMissingPlan prints each missing path and the denominator.
func renderMissingPlan(rows []missingPlanRow) string {
	var b strings.Builder
	present, missing, chunks := 0, 0, 0
	for _, r := range rows {
		if r.Decision == missingPresent {
			present++
			continue
		}
		missing++
		chunks += r.Chunks
		fmt.Fprintf(&b, "missing  %s (survivor %s): %d chunks\n", r.Path, r.Survivor, r.Chunks)
		if len(r.MovedTo) > 0 {
			fmt.Fprintf(&b, "           moved to? %s\n", strings.Join(r.MovedTo, ", "))
		}
	}
	fmt.Fprintf(&b, "\nexamined %d paths: %d present, %d missing (%d chunks)\n", len(rows), present, missing, chunks)
	return b.String()
}

// stillAsPlanned reports whether a missing row may be retired now: its path's
// survivor is still the upload the plan recorded. A re-ingest since the plan
// makes the path live again; an empty survivor means nothing is left to retire.
func stillAsPlanned(row missingPlanRow, survivorNow string) bool {
	return row.Survivor != "" && survivorNow == row.Survivor
}

// onDefaultBranch refuses a checkout whose HEAD is not the remote's default
// branch (refs/remotes/origin/HEAD): a path can exist on one branch and not
// another, and a scope names a repository, not a branch.
func onDefaultBranch(root string) error {
	def, err := exec.Command("git", "-C", root, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return fmt.Errorf("the checkout at %s has no refs/remotes/origin/HEAD, so its default branch is unknown (git remote set-head origin --auto), or pass --allow-branch", root)
	}
	want := strings.TrimPrefix(strings.TrimSpace(string(def)), "refs/remotes/origin/")
	cur, err := exec.Command("git", "-C", root, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("read the checkout's branch: %w", err)
	}
	if got := strings.TrimSpace(string(cur)); got != want {
		return fmt.Errorf("the checkout at %s is on %q, not the default branch %q: a document ingested from %q and absent here may still exist there; check out %q, or pass --allow-branch", root, got, want, want, want)
	}
	return nil
}

var (
	missingDocsProject     string
	missingDocsScope       string
	missingDocsRoot        string
	missingDocsApply       bool
	missingDocsAnyScope    bool
	missingDocsAllowBranch bool
)

var memorySupersedeMissingCmd = &cobra.Command{
	Use:   "supersede-missing-documents",
	Short: "Retire path-identity document chunks whose file is no longer in the checkout",
	Long: `Re-ingesting a document under the same path supersedes its earlier versions,
but nothing retires a document whose file was deleted or moved. For each
path document in the scope, this verb looks the path up in the checkout at
--root (tracked files plus untracked files git does not ignore):

  the path is in the checkout   -> left alone
  the path is not in it         -> retired

A missing row also lists, for information, tracked files with the same base
name that have a live version ("moved to?"); no decision uses it. Re-ingest
moved documents under their new path first, or recall loses them until you do.

The checkout must be on the remote's default branch (--allow-branch to
override), and its scope must be --scope. A dry run by default: nothing is
written without --apply. Under --apply a path re-ingested since the plan is
skipped. The retirement records no epoch, so a corpus rollback does not
restore it.

Always runs against the live daemon's chunk store.`,
	RunE: runMemorySupersedeMissing,
}

func init() {
	f := memorySupersedeMissingCmd.Flags()
	f.StringVarP(&missingDocsProject, "project", "p", "", "Project ID (required)")
	f.StringVar(&missingDocsScope, "scope", "", "Repo scope the documents were ingested under (required)")
	f.StringVar(&missingDocsRoot, "root", "", "A path inside the checkout the documents come from (required)")
	f.BoolVar(&missingDocsApply, "apply", false, "Write the plan; without it this is a dry run")
	f.BoolVar(&missingDocsAnyScope, "allow-scope-mismatch", false,
		"Run although --scope is not the scope the checkout at --root resolves to (a pinned or retagged scope)")
	f.BoolVar(&missingDocsAllowBranch, "allow-branch", false, "Run although the checkout is not on the remote's default branch")
	_ = memorySupersedeMissingCmd.MarkFlagRequired("project")
	_ = memorySupersedeMissingCmd.MarkFlagRequired("scope")
	_ = memorySupersedeMissingCmd.MarkFlagRequired("root")
	memoryCmd.AddCommand(memorySupersedeMissingCmd)
}

func runMemorySupersedeMissing(cmd *cobra.Command, _ []string) error {
	derived, err := checkoutScope(missingDocsRoot)
	if err != nil {
		return err
	}
	if derived != missingDocsScope && !missingDocsAnyScope {
		return fmt.Errorf("the checkout at --root resolves to scope %q, not %q; point --root at the repository those chunks came from, "+
			"or pass --allow-scope-mismatch if the scope was pinned or retagged", derived, missingDocsScope)
	}
	if !missingDocsAllowBranch {
		if err := onDefaultBranch(missingDocsRoot); err != nil {
			return err
		}
	}
	tracked, err := trackedFiles(missingDocsRoot)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
	defer cancel()
	db, err := openVornikDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	repo := memory.NewRepository(db)
	docs, err := repo.PathDocuments(ctx, missingDocsProject, missingDocsScope)
	if err != nil {
		return err
	}
	survivor := func(p string) (string, error) {
		return repo.DocumentSurvivor(ctx, missingDocsProject, missingDocsScope, p)
	}
	rows, err := planMissingDocumentsWith(docs, tracked, survivor)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprint(out, renderMissingPlan(rows))
	if !missingDocsApply {
		_, _ = fmt.Fprintln(out, "(dry run; nothing written. Re-run with --apply to retire the missing paths.)")
		return nil
	}
	total, paths, skipped, err := applyMissingPlan(rows, survivor, func(p string) (int, error) {
		return repo.SupersedeMissingDocument(ctx, missingDocsProject, missingDocsScope, p)
	}, out)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Retired %d chunks across %d paths; skipped %d.\n", total, paths, skipped)
	return nil
}

// applyMissingPlan retires each missing row whose survivor is still the one
// the plan recorded, and skips the rest.
func applyMissingPlan(rows []missingPlanRow, survivor func(string) (string, error), retire func(string) (int, error), out io.Writer) (total, paths, skipped int, err error) {
	for _, r := range rows {
		if r.Decision != missingMissing {
			continue
		}
		now, err := survivor(r.Path)
		if err != nil {
			return total, paths, skipped, fmt.Errorf("retired %d chunks across %d paths before failing on %s: %w (re-running is safe)", total, paths, r.Path, err)
		}
		if !stillAsPlanned(r, now) {
			skipped++
			_, _ = fmt.Fprintf(out, "skipped  %s: it changed since the plan (re-ingested or already retired)\n", r.Path)
			continue
		}
		n, err := retire(r.Path)
		if err != nil {
			return total, paths, skipped, fmt.Errorf("retired %d chunks across %d paths before failing on %s: %w (re-running is safe)", total, paths, r.Path, err)
		}
		total += n
		paths++
	}
	return total, paths, skipped, nil
}
