package cli

// `vornikctl memory regraph` — operator-driven backfill of isolated
// knowledge-graph entities. Flips needs_graph_extraction = TRUE on
// every chunk in --project that produced zero published edges so the
// daemon's KG worker re-runs the extraction pipeline against them
// with whatever logic is current.
//
// Use case: after a pipeline-logic fix (e.g. the 2026-05-25
// evidence-substring normalisation, commit ed1a501) the operator
// wants the existing isolated entities (48% of the live KG at audit
// time) to actually benefit, not just future ingest. Without this
// surface they wait for new chunks to arrive.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	regraphProject       string
	regraphDryRun        bool
	regraphJSON          bool
	regraphQuarantined   bool
	regraphErrorContains string
)

var memoryRegraphCmd = &cobra.Command{
	Use:   "regraph",
	Short: "Re-flag chunks with zero edges (or quarantined chunks) so the KG worker reprocesses them",
	Long: `Re-flag every chunk in --project that produced zero published
edges, so the daemon's KG extraction worker picks them up on its next
tick and re-runs the four-stage pipeline (extractor → resolver →
relationship → validator) with whatever logic is currently shipping.

Use after a KG-pipeline fix to make existing isolated entities benefit
from the change. Without this, the fix only helps NEW chunks.

Idempotent: re-running against the same project re-flags the same
chunk set minus any that the latest pass DID manage to extract edges
from. --dry-run reports the candidate count without writing.

--quarantined selects a different set: only QUARANTINED chunks (the
worker gave up after its failure bound, graph_extraction_attempts > 0
with the flag cleared), and re-flags them with the attempt counter
reset to 0, so each gets a full set of attempts under the current
code. --error-contains narrows to chunks whose last recorded error
contains the given text (literal, case-sensitive). --project is
optional here; without it every project is examined, which needs a
key that is not project-scoped. The report lists, per project, the
quarantined chunks examined, the ones matched and the ones
re-flagged. A chunk that is not quarantined is never touched.

Examples:
  vornikctl memory regraph --project assistant --dry-run
  vornikctl memory regraph --project assistant
  vornikctl memory regraph --project assistant --json
  vornikctl memory regraph --quarantined --error-contains "invalid byte sequence" --dry-run
  vornikctl memory regraph --quarantined --error-contains "invalid byte sequence"`,
	RunE: runMemoryRegraph,
}

func init() {
	memoryRegraphCmd.Flags().StringVarP(&regraphProject, "project", "p", "", "Project ID (required, except with --quarantined)")
	memoryRegraphCmd.Flags().BoolVar(&regraphDryRun, "dry-run", false, "Report candidate count without writing")
	memoryRegraphCmd.Flags().BoolVar(&regraphJSON, "json", false, "Emit JSON summary instead of human-readable output")
	memoryRegraphCmd.Flags().BoolVar(&regraphQuarantined, "quarantined", false, "Re-flag only quarantined chunks, resetting their attempt counter (all projects unless --project)")
	memoryRegraphCmd.Flags().StringVar(&regraphErrorContains, "error-contains", "", "With --quarantined: only chunks whose last error contains this text (literal, case-sensitive)")
	memoryCmd.AddCommand(memoryRegraphCmd)
}

// memoryRegraphResponse mirrors api.MemoryGraphReflagResult on the
// client side. Kept here so the CLI stays a self-contained tool.
type memoryRegraphResponse struct {
	Project   string `json:"project"`
	DryRun    bool   `json:"dryRun"`
	ReFlagged int    `json:"reFlagged"`
}

// validateRegraphFlags rejects flag combinations the server would otherwise
// silently ignore or refuse.
func validateRegraphFlags(project, errorContains string, quarantined bool) error {
	if !quarantined && errorContains != "" {
		return errors.New("--error-contains needs --quarantined")
	}
	if !quarantined && project == "" {
		return errors.New("--project is required")
	}
	return nil
}

func runMemoryRegraph(_ *cobra.Command, _ []string) error {
	if err := validateRegraphFlags(regraphProject, regraphErrorContains, regraphQuarantined); err != nil {
		return err
	}
	if regraphQuarantined {
		return runMemoryRegraphQuarantined(regraphProject, regraphErrorContains, regraphDryRun, regraphJSON, os.Stdout)
	}
	q := url.Values{}
	q.Set("project", regraphProject)
	if regraphDryRun {
		q.Set("dry_run", "true")
	}
	path := "/api/v1/memory/regraph?" + q.Encode()

	raw, err := postJSON(path, nil)
	if err != nil {
		return fmt.Errorf("memory regraph: %w", err)
	}
	var resp memoryRegraphResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("memory regraph: parse response: %w", err)
	}

	if regraphJSON {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	verb := "re-flagged"
	if resp.DryRun {
		verb = "would re-flag"
	}
	fmt.Printf("%s %d chunks in project %q for KG re-extraction.\n", verb, resp.ReFlagged, resp.Project)
	if resp.DryRun {
		fmt.Println("(dry-run: no changes written. re-run without --dry-run to apply.)")
	} else if resp.ReFlagged > 0 {
		fmt.Println("The KG worker will pick them up on its next tick (typically <1 minute).")
		fmt.Println("Watch progress: `vornikctl memory stats --project " + resp.Project + "` or the vornik_memory_graph_chunks_pending metric.")
	}
	return nil
}

// memoryRegraphQuarantinedRow / Response mirror
// api.MemoryGraphQuarantineReflagResult on the client side.
type memoryRegraphQuarantinedRow struct {
	Project     string `json:"project,omitempty"`
	Quarantined int    `json:"quarantined"`
	Matched     int    `json:"matched"`
	ReFlagged   int    `json:"reflagged"`
}

type memoryRegraphQuarantinedResponse struct {
	Project       string                        `json:"project,omitempty"`
	ErrorContains string                        `json:"errorContains,omitempty"`
	DryRun        bool                          `json:"dryRun"`
	Projects      []memoryRegraphQuarantinedRow `json:"projects"`
	Totals        memoryRegraphQuarantinedRow   `json:"totals"`
}

// runMemoryRegraphQuarantined drives POST /api/v1/memory/regraph/quarantined
// (knowledge-graph LLD §3.3b): quarantined chunks only, attempt counter
// reset, the per-project denominator printed beside the selection.
func runMemoryRegraphQuarantined(project, errorContains string, dryRun, asJSON bool, w io.Writer) error {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if errorContains != "" {
		q.Set("error_contains", errorContains)
	}
	if dryRun {
		q.Set("dry_run", "true")
	}
	path := "/api/v1/memory/regraph/quarantined"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	raw, err := postJSON(path, nil)
	if err != nil {
		return fmt.Errorf("memory regraph --quarantined: %w", err)
	}
	var resp memoryRegraphQuarantinedResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("memory regraph --quarantined: parse response: %w", err)
	}
	if asJSON {
		return json.NewEncoder(w).Encode(resp)
	}

	scope := "every project"
	if resp.Project != "" {
		scope = fmt.Sprintf("project %q", resp.Project)
	}
	match := "any error"
	if resp.ErrorContains != "" {
		match = fmt.Sprintf("error containing %q", resp.ErrorContains)
	}
	_, _ = fmt.Fprintf(w, "Quarantined KG chunks in %s, selecting %s:\n", scope, match)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROJECT\tQUARANTINED\tMATCHED\tRE-FLAGGED")
	for _, r := range resp.Projects {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\n", r.Project, r.Quarantined, r.Matched, r.ReFlagged)
	}
	_, _ = fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%d\n", resp.Totals.Quarantined, resp.Totals.Matched, resp.Totals.ReFlagged)
	_ = tw.Flush()
	if resp.DryRun {
		_, _ = fmt.Fprintf(w, "would re-flag %d of %d quarantined chunks (attempt counter reset to 0).\n",
			resp.Totals.Matched, resp.Totals.Quarantined)
		_, _ = fmt.Fprintln(w, "(dry-run: no changes written. re-run without --dry-run to apply.)")
		return nil
	}
	_, _ = fmt.Fprintf(w, "re-flagged %d of %d quarantined chunks (attempt counter reset to 0).\n",
		resp.Totals.ReFlagged, resp.Totals.Quarantined)
	if resp.Totals.ReFlagged > 0 {
		_, _ = fmt.Fprintln(w, "The KG worker picks them up on its next tick. Watch progress: the vornik_memory_graph_chunks_pending metric.")
	}
	return nil
}
