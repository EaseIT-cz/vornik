package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/memory"
)

// Legacy document cleanup (memory rollback x supersession design,
// amendment 2026-09-26, A.5). Uploads made before document paths existed
// carry only a bare file name, so document supersession never matches them.
// This verb retires them, one name at a time, only when the name belongs to
// exactly one tracked file in the operator's checkout AND that file's
// path-identity version is live. The file list comes from the checkout, which
// is why this is a client verb: the daemon cannot see repositories and never
// guesses a basename's owner.

const (
	legacySupersede     = "supersede"
	legacyAmbiguous     = "ambiguous"
	legacyMissing       = "missing"
	legacyNotReingested = "not-reingested"
	// legacyKeepNewest is a file at the repository root: its path IS its bare
	// name, so its legacy chunks are its path-identity version. Only the older
	// uploads are retired, exactly as document supersession would; retiring the
	// name outright would delete the document (found by the 2026-09-26 dry run
	// against the reference host's store).
	legacyKeepNewest = "keep-newest"
	// legacyIncomplete is a name whose live path version predates whole-version
	// ingest (design A.7): it was de-duplicated against the bare-name versions
	// and may lack sections only they hold, so nothing is retired until it is
	// re-ingested.
	legacyIncomplete = "incomplete"
)

type legacyPlanRow struct {
	Name       string
	Chunks     int
	ByDate     map[string]int
	Decision   string
	Path       string   // the single tracked file with this name, when there is one
	Candidates []string // every tracked file with this name, when there are several
	LiveUnder  []string // ambiguous rows: the candidates that are live under their own path
	Survivor   string   // the path-identity upload that stays live
	Retire     int      // keep-newest rows: the older chunks that would be retired
}

// planLegacyDocumentsWith decides, per bare name, whether it can be retired.
// survivor returns the live path-identity upload for a path, or ""; whole
// reports whether that version was stored whole (design A.7).
func planLegacyDocumentsWith(docs []memory.LegacyDocument, tracked []string,
	survivor func(string) (string, error), whole func(string) (bool, error)) ([]legacyPlanRow, error) {
	byBase := map[string][]string{}
	for _, f := range tracked {
		byBase[path.Base(f)] = append(byBase[path.Base(f)], f)
	}
	rows := make([]legacyPlanRow, 0, len(docs))
	for _, d := range docs {
		row := legacyPlanRow{Name: d.Name, Chunks: d.Chunks, ByDate: d.ByDate}
		switch matches := byBase[d.Name]; len(matches) {
		case 0:
			row.Decision = legacyMissing
		case 1:
			row.Path = matches[0]
			s, err := survivor(row.Path)
			if err != nil {
				return nil, fmt.Errorf("look up %s: %w", row.Path, err)
			}
			row.Survivor = s
			switch {
			case s == "" && row.Path != d.Name:
				row.Decision = legacyNotReingested
			default:
				complete, err := whole(row.Path)
				if err != nil {
					return nil, fmt.Errorf("check %s: %w", row.Path, err)
				}
				switch {
				case !complete:
					row.Decision = legacyIncomplete
				case row.Path == d.Name:
					row.Decision = legacyKeepNewest
				default:
					row.Decision = legacySupersede
				}
			}
		default:
			row.Decision = legacyAmbiguous
			row.Candidates = append([]string(nil), matches...)
			sort.Strings(row.Candidates)
			for _, c := range row.Candidates {
				s, err := survivor(c)
				if err != nil {
					return nil, fmt.Errorf("look up %s: %w", c, err)
				}
				if s != "" {
					row.LiveUnder = append(row.LiveUnder, c)
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// renderLegacyPlan prints the per-name blast radius and the denominator:
// how many names were examined, not only how many are retired.
func renderLegacyPlan(rows []legacyPlanRow) string {
	var b strings.Builder
	counts := map[string]int{}
	retired := 0
	for _, r := range rows {
		counts[r.Decision]++
		switch r.Decision {
		case legacySupersede:
			retired += r.Chunks
			fmt.Fprintf(&b, "supersede  %s -> %s (survivor %s): %d chunks\n", r.Name, r.Path, r.Survivor, r.Chunks)
			dates := make([]string, 0, len(r.ByDate))
			for d := range r.ByDate {
				dates = append(dates, d)
			}
			sort.Strings(dates)
			for _, d := range dates {
				fmt.Fprintf(&b, "             %s: %d\n", d, r.ByDate[d])
			}
		case legacyKeepNewest:
			retired += r.Retire
			fmt.Fprintf(&b, "keep-new   %s (a root file: its name is its path; keeps %s): %d older chunks\n", r.Name, r.Survivor, r.Retire)
		case legacyAmbiguous:
			fmt.Fprintf(&b, "ambiguous  %s: %d tracked files share the name, left alone: %s\n", r.Name, len(r.Candidates), strings.Join(r.Candidates, ", "))
			if len(r.LiveUnder) > 0 {
				fmt.Fprintf(&b, "             live under their path: %s\n", strings.Join(r.LiveUnder, ", "))
			}
		case legacyMissing:
			fmt.Fprintf(&b, "missing    %s: no tracked file has this name, left alone\n", r.Name)
		case legacyIncomplete:
			fmt.Fprintf(&b, "incomplete %s -> %s: its path version predates whole-version ingest, left alone; re-ingest it, then run this again\n", r.Name, r.Path)
		case legacyNotReingested:
			fmt.Fprintf(&b, "waiting    %s -> %s: not re-ingested under its path yet, left alone\n", r.Name, r.Path)
		}
	}
	keepChunks := 0
	for _, r := range rows {
		if r.Decision == legacyKeepNewest {
			keepChunks += r.Retire
		}
	}
	fmt.Fprintf(&b, "\nexamined %d names: supersede %d (%d chunks), keep newest %d (%d chunks), incomplete %d, ambiguous %d, missing from the checkout %d, not yet re-ingested %d\n",
		len(rows), counts[legacySupersede], retired-keepChunks, counts[legacyKeepNewest], keepChunks,
		counts[legacyIncomplete], counts[legacyAmbiguous], counts[legacyMissing], counts[legacyNotReingested])
	return b.String()
}

// trackedFiles lists the checkout's working-tree files, relative to the
// repository root: tracked, plus untracked files git does not ignore. That is
// the set /rag-ingest can send a document path for, so an uncommitted document
// is still retire-eligible.
func trackedFiles(root string) ([]string, error) {
	top, err := exec.Command("git", "-C", root, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git checkout: %w", root, err)
	}
	out, err := exec.Command("git", "-C", strings.TrimSpace(string(top)), "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	seen := map[string]bool{}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 && !seen[string(f)] {
			seen[string(f)] = true
			files = append(files, string(f))
		}
	}
	return files, nil
}

// checkoutScope derives the repo scope of the checkout at root exactly as the
// companion plugin does: the normalised origin remote, else the repository's
// folder name.
func checkoutScope(root string) (string, error) {
	top, err := exec.Command("git", "-C", root, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git checkout: %w", root, err)
	}
	topDir := strings.TrimSpace(string(top))
	url, err := exec.Command("git", "-C", topDir, "config", "--get", "remote.origin.url").Output()
	if err != nil || strings.TrimSpace(string(url)) == "" {
		return path.Base(topDir), nil
	}
	return normalizeRemote(strings.TrimSpace(string(url))), nil
}

// normalizeRemote turns a git remote URL into the host/path scope token, with
// the same rules as the companion plugin's normalize_remote: strip ".git",
// the scheme, and a leading "user@", then "host:path" becomes "host/path".
func normalizeRemote(url string) string {
	url = strings.TrimSuffix(url, ".git")
	for _, pre := range []string{"https://", "http://", "ssh://"} {
		url = strings.TrimPrefix(url, pre)
	}
	if at := strings.Index(url, "@"); at > 0 {
		sep := -1
		for _, i := range []int{strings.Index(url, ":"), strings.Index(url, "/")} {
			if i != -1 && (sep == -1 || i < sep) {
				sep = i
			}
		}
		if sep == -1 || at < sep {
			url = url[at+1:]
		}
	}
	if i := strings.Index(url, ":"); i != -1 && !strings.Contains(url[:i], "/") {
		url = url[:i] + "/" + url[i+1:]
	}
	return url
}

var (
	legacyDocsProject  string
	legacyDocsScope    string
	legacyDocsRoot     string
	legacyDocsApply    bool
	legacyDocsAnyScope bool
)

var memorySupersedeLegacyCmd = &cobra.Command{
	Use:   "supersede-legacy-documents",
	Short: "Retire bare-name document chunks that a path-identity re-ingest has replaced",
	Long: `Documents uploaded before document paths existed carry only a bare file
name, so re-ingesting them never superseded the earlier versions. For each
bare name in the scope, this verb looks the name up in the checkout at --root:

  exactly one tracked file, and that path has a live re-ingest  -> superseded
  two or more tracked files with the name (e.g. index.md)       -> left alone
  no tracked file with the name                                 -> left alone
  one file, but not yet re-ingested under its path              -> left alone
  the path version predates whole-version ingest                -> left alone
                                                                   (re-ingest first)
  a file at the repository root (its name is its path)          -> older uploads
                                                                   superseded, the
                                                                   newest kept

Re-ingest the documents with a current companion plugin first, then run this.
A dry run by default: it prints, per name, the surviving upload and the chunks
it would retire per ingest date. Nothing is written without --apply. The
retirement records no epoch, so a corpus rollback does not restore it.

Always runs against the live daemon's chunk store.`,
	RunE: runMemorySupersedeLegacy,
}

func init() {
	memorySupersedeLegacyCmd.Flags().StringVarP(&legacyDocsProject, "project", "p", "", "Project ID (required)")
	memorySupersedeLegacyCmd.Flags().StringVar(&legacyDocsScope, "scope", "", "Repo scope the documents were ingested under (required)")
	memorySupersedeLegacyCmd.Flags().StringVar(&legacyDocsRoot, "root", "", "A path inside the checkout the documents come from (required)")
	memorySupersedeLegacyCmd.Flags().BoolVar(&legacyDocsApply, "apply", false, "Write the plan; without it this is a dry run")
	memorySupersedeLegacyCmd.Flags().BoolVar(&legacyDocsAnyScope, "allow-scope-mismatch", false,
		"Run although --scope is not the scope the checkout at --root resolves to (a pinned or retagged scope)")
	_ = memorySupersedeLegacyCmd.MarkFlagRequired("project")
	_ = memorySupersedeLegacyCmd.MarkFlagRequired("scope")
	_ = memorySupersedeLegacyCmd.MarkFlagRequired("root")
	memoryCmd.AddCommand(memorySupersedeLegacyCmd)
}

func runMemorySupersedeLegacy(cmd *cobra.Command, _ []string) error {
	// --root decides which file owns a name; --scope decides whose chunks are
	// retired. A checkout of one repository against another's scope would
	// retire the wrong chunks, so they must agree.
	derived, err := checkoutScope(legacyDocsRoot)
	if err != nil {
		return err
	}
	if derived != legacyDocsScope && !legacyDocsAnyScope {
		return fmt.Errorf("the checkout at --root resolves to scope %q, not %q; point --root at the repository those chunks came from, "+
			"or pass --allow-scope-mismatch if the scope was pinned or retagged", derived, legacyDocsScope)
	}
	tracked, err := trackedFiles(legacyDocsRoot)
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
	docs, err := repo.LegacyDocuments(ctx, legacyDocsProject, legacyDocsScope)
	if err != nil {
		return err
	}
	whole := func(p string) (bool, error) {
		return repo.DocumentVersionIsWhole(ctx, legacyDocsProject, legacyDocsScope, p)
	}
	rows, err := planLegacyDocumentsWith(docs, tracked, func(p string) (string, error) {
		return repo.DocumentSurvivor(ctx, legacyDocsProject, legacyDocsScope, p)
	}, whole)
	if err != nil {
		return err
	}
	for i := range rows {
		if rows[i].Decision != legacyKeepNewest {
			continue
		}
		if rows[i].Retire, err = repo.DocumentOlderChunks(ctx, legacyDocsProject, legacyDocsScope, rows[i].Path); err != nil {
			return err
		}
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprint(out, renderLegacyPlan(rows))
	if !legacyDocsApply {
		_, _ = fmt.Fprintln(out, "(dry run; nothing written. Re-run with --apply to retire the names marked supersede.)")
		return nil
	}
	total, names := 0, 0
	for _, r := range rows {
		var n int
		switch r.Decision {
		case legacySupersede:
			// Re-check immediately before the irreversible retire: the
			// survivor may have been rolled back since the plan was made.
			s, lookupErr := repo.DocumentSurvivor(ctx, legacyDocsProject, legacyDocsScope, r.Path)
			if lookupErr != nil {
				return fmt.Errorf("retired %d chunks across %d names before failing on %s: %w (re-running is safe: retired names are no longer listed)", total, names, r.Name, lookupErr)
			}
			if s == "" {
				_, _ = fmt.Fprintf(out, "skipped    %s: %s is no longer live, so its old versions stay\n", r.Name, r.Path)
				continue
			}
			if complete, werr := whole(r.Path); werr != nil || !complete {
				_, _ = fmt.Fprintf(out, "skipped    %s: %s is no longer a whole version, so its old versions stay\n", r.Name, r.Path)
				continue
			}
			n, err = repo.SupersedeLegacyDocument(ctx, legacyDocsProject, legacyDocsScope, r.Name)
		case legacyKeepNewest:
			// No re-check here, deliberately: this row was planned only after
			// its version proved whole, and SupersedeDocument itself keeps the
			// newest live upload at call time, so it cannot retire the document
			// even if state moved since the plan. Skipping on a re-check would
			// only leave old versions live (code review 2026-09-26).
			n, err = repo.SupersedeDocument(ctx, legacyDocsProject, legacyDocsScope, r.Path, "")
		default:
			continue
		}
		if err != nil {
			return fmt.Errorf("retired %d chunks across %d names before failing on %s: %w (re-running is safe: retired names are no longer listed)", total, names, r.Name, err)
		}
		total += n
		names++
	}
	_, _ = fmt.Fprintf(out, "Superseded %d chunks.\n", total)
	return nil
}
