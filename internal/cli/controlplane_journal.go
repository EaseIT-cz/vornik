package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// `vornikctl control-plane journal list|resolve` (config-apply journal design
// §12, GitHub #285): the one supported way out of a DRIFT row, which blocks
// its projects until an operator decides. Both need an admin-class key.

var cpJournalCmd = &cobra.Command{
	Use:   "journal",
	Short: "Config-apply journal: list open rows, resolve a row parked in DRIFT (admin key)",
}

var cpJournalListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the open config-apply journal rows (DRIFT rows block their projects)",
	RunE:  runCPJournalList,
}

var cpJournalResolveCmd = &cobra.Command{
	Use:   "resolve <journal-id>",
	Short: "Resolve a DRIFT row: --keep the files as they are, or --restore the prior content",
	Long: `Resolve a config-apply journal row parked in DRIFT, which unblocks its projects.

--keep accepts the files as they are and writes nothing. When every file holds
the content the proposal wrote and the proposal is APPROVED, the row and the
proposal are marked APPLIED. When the proposal is no longer APPROVED (for
example REJECTED), the row is closed as FAILED with a note that you kept the
files, and the proposal is left as it is. When a file was edited by hand, the
row is marked FAILED and an APPROVED proposal is retired.

--restore writes back the content each file had before the apply (overwriting
a hand edit), removes a file the apply created when it still holds what the
apply wrote, and reloads. The row is marked REVERTED and the proposal stays
APPROVED.

Requires an admin key. Every resolve is audited. Resolving an already
resolved row the same way changes nothing.`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error { return runCPJournalResolve(args[0]) },
}

var cpJournalKeep, cpJournalRestore bool

func init() {
	cpJournalResolveCmd.Flags().BoolVar(&cpJournalKeep, "keep", false, "Accept the files as they are (no file is written)")
	cpJournalResolveCmd.Flags().BoolVar(&cpJournalRestore, "restore", false, "Write the prior content back (overwrites a hand edit)")
	cpJournalResolveCmd.Flags().StringVar(&cpActor, "author", "", "Who resolves it, recorded on the row and in the audit (defaults to $USER)")
	cpJournalCmd.AddCommand(cpJournalListCmd, cpJournalResolveCmd)
	controlPlaneCmd.AddCommand(cpJournalCmd)
}

type cpJournalRow struct {
	ID               string   `json:"id"`
	ProposalID       string   `json:"proposalId"`
	State            string   `json:"state"`
	AffectedProjects []string `json:"affectedProjects"`
	Failure          string   `json:"failure"`
}

type cpJournalResolveOut struct {
	JournalID        string   `json:"journalId"`
	Mode             string   `json:"mode"`
	State            string   `json:"state"`
	ProposalStatus   string   `json:"proposalStatus"`
	Changed          bool     `json:"changed"`
	AffectedProjects []string `json:"affectedProjects"`
	LeftInPlace      []string `json:"leftInPlace"`
	ReloadError      string   `json:"reloadError"`
	AuditError       string   `json:"auditError"`
	Outcome          string   `json:"outcome"`
	Message          string   `json:"message"`
}

func runCPJournalList(_ *cobra.Command, _ []string) error {
	resp, err := ClientFromEnv().Get("/api/v1/operator/journal")
	if err != nil {
		return fmt.Errorf("journal list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Rows []cpJournalRow `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if len(out.Rows) == 0 {
		fmt.Println("No open config-apply journal rows.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATE\tPROPOSAL\tPROJECTS\tFAILURE")
	for _, r := range out.Rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.State, r.ProposalID, strings.Join(r.AffectedProjects, ","), r.Failure)
	}
	return tw.Flush()
}

func runCPJournalResolve(id string) error {
	if cpJournalKeep == cpJournalRestore {
		return errors.New("journal resolve needs exactly one of --keep or --restore")
	}
	mode := "keep"
	if cpJournalRestore {
		mode = "restore"
	}
	resp, err := ClientFromEnv().Post("/api/v1/operator/journal/"+id+"/resolve",
		map[string]any{"mode": mode, "actor": resolveCPActor()})
	if err != nil {
		return fmt.Errorf("journal resolve: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var out cpJournalResolveOut
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if !out.Changed {
		fmt.Printf("Journal %s was already resolved (%s); nothing changed. Row %s, proposal %s.\n", out.JournalID, out.Mode, out.State, out.ProposalStatus)
	} else {
		fmt.Printf("Resolved journal %s (%s): row %s, proposal %s.\n", out.JournalID, out.Mode, out.State, out.ProposalStatus)
		if out.Message != "" {
			fmt.Println(out.Message)
		}
		fmt.Printf("Projects no longer blocked: %s\n", strings.Join(out.AffectedProjects, ", "))
	}
	if len(out.LeftInPlace) > 0 {
		fmt.Printf("Created files left in place (they changed after the apply): %s\n", strings.Join(out.LeftInPlace, ", "))
	}
	if out.ReloadError != "" {
		fmt.Printf("Warning: the reload after the restore failed: %s\n", out.ReloadError)
	}
	if out.AuditError != "" {
		return fmt.Errorf("the resolve stands, but its audit row was not written: %s", out.AuditError)
	}
	return nil
}
