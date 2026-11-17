package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// `vornikctl account transitions list|recover` (design 22 §22.2.16b, GitHub
// #669 item 3): the controlled recovery of a busy account-transition row
// whose holder is gone, which was a hand-run SQL CAS. Admin key only.

var accountTransitionsCmd = &cobra.Command{
	Use:   "transitions",
	Short: "Credential transitions of agent namespaces: list busy ones, recover an abandoned one (admin key)",
	Long: `An account transition serialises a change to an agent namespace's (or the
daemon's) credentials. A row left busy by a daemon that crashed mid-change
blocks every later change in its scope until it is recovered.

These are credential transitions, not the user accounts the rest of
'vornikctl account' manages.`,
}

var accountTransitionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List busy transitions, with the full owner recover needs",
	RunE:  func(*cobra.Command, []string) error { return runAccountTransitionsList() },
}

var accountTransitionsRecoverCmd = &cobra.Command{
	Use:   "recover <scope> <epoch> <owner>",
	Short: "Clear one busy transition whose holder is gone",
	Long: `Clear one busy transition row whose holder is gone: the epoch advances and
busy clears, in one compare-and-swap on exactly that scope, epoch and owner
(copy them from 'vornikctl account transitions list').

Before you run it: stop or drain every other daemon node's account and
config writers. This command cannot see a holder on another node. On this
daemon it refuses a ticket the daemon holds, and it refuses while an open
config-apply journal row could touch the scope: resolve that first with
'vornikctl control-plane journal resolve'.

Suspended grants stay suspended; confirm them on the approver device. Every
call is audited, a refused one included. Requires an admin key.`,
	Args: cobra.ExactArgs(3),
	RunE: func(_ *cobra.Command, args []string) error {
		return runAccountTransitionsRecover(args[0], args[1], args[2])
	},
}

func init() {
	accountTransitionsCmd.AddCommand(accountTransitionsListCmd, accountTransitionsRecoverCmd)
	accountCmd.AddCommand(accountTransitionsCmd)
}

func runAccountTransitionsList() error {
	resp, err := ClientFromEnv().Get("/api/v1/operator/account-transitions")
	if err != nil {
		return fmt.Errorf("account transitions list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Rows []struct {
			Scope      string `json:"scope"`
			Epoch      int64  `json:"epoch"`
			Owner      string `json:"owner"`
			Operation  string `json:"operation"`
			AgeSeconds int64  `json:"ageSeconds"`
			HeldHere   bool   `json:"heldHere"`
		} `json:"rows"`
		Examined int `json:"examined"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if len(out.Rows) == 0 {
		fmt.Printf("No busy account transitions (%d rows examined).\n", out.Examined)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SCOPE\tEPOCH\tOPERATION\tAGE\tHELD HERE\tOWNER")
	for _, r := range out.Rows {
		held := "no"
		if r.HeldHere {
			held = "yes (running)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%ds\t%s\t%s\n", r.Scope, r.Epoch, r.Operation, r.AgeSeconds, held, r.Owner)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("%d busy of %d rows examined.\n", len(out.Rows), out.Examined)
	return nil
}

func runAccountTransitionsRecover(scope, epochArg, owner string) error {
	epoch, err := strconv.ParseInt(epochArg, 10, 64)
	if err != nil || epoch < 0 {
		return fmt.Errorf("epoch must be a whole number, as list prints it: %q", epochArg)
	}
	resp, err := ClientFromEnv().Post("/api/v1/operator/account-transitions/recover",
		map[string]any{"scope": scope, "epoch": epoch, "owner": owner})
	if err != nil {
		return fmt.Errorf("account transitions recover: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		EpochBefore int64  `json:"epochBefore"`
		EpochAfter  int64  `json:"epochAfter"`
		Operation   string `json:"operation"`
		Note        string `json:"note"`
		AuditError  string `json:"auditError"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	fmt.Printf("Recovered %s: %s at epoch %d cleared; epoch is now %d. %s.\n", scope, out.Operation, out.EpochBefore, out.EpochAfter, out.Note)
	if out.AuditError != "" {
		return fmt.Errorf("the recovery stands, but its audit row was not written: %s", out.AuditError)
	}
	return nil
}
