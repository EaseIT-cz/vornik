package cli

// vornikctl pricing check (GitHub #776, config-tree drift design, amendment
// 2026-11-19): parse a pricing file the way the daemon does at start, so
// config-deploy installs a merged pricing.yaml only when the daemon will load
// it. NewContainer refuses to start on a pricing file that does not parse.

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/pricing"
)

var pricingCheckCmd = &cobra.Command{
	Use:   "check <file>",
	Short: "Check that a pricing file parses the way the daemon loads it",
	Long: `Parse a pricing file with the daemon's own loader and print how many model
ids it prices. Exits non-zero when the file is missing or does not parse (a
duplicate model id, a broken row), because the daemon would refuse to start on
it. The upgrade's config deploy runs it before installing a pricing.yaml it
added shipped rows to.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPricingCheck(cmd.OutOrStdout(), args[0])
	},
}

func init() { pricingCmd.AddCommand(pricingCheckCmd) }

// runPricingCheck refuses a missing file, which pricing.Load would read as an
// empty table, then loads it and reports the id count.
func runPricingCheck(w io.Writer, path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("pricing check: %w", err)
	}
	t, err := pricing.Load(path)
	if err != nil {
		return fmt.Errorf("pricing check: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s: parses; %d model id(s) priced\n", path, len(t.IDs()))
	return err
}
