package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

// daemonReleaseCmd releases a held start (GitHub #586): the daemon reloads
// its config files, removes the start-held marker and starts its work.
// Exit 0 released (or not held), 1 on any error.
// see LLD § https://docs.vornik.io §13.4
var daemonReleaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Release a daemon that started held (start its work)",
	Long: `Release a daemon that started held by its start-held marker: it reloads
the config files, removes the marker, starts its work (scheduler, autonomy,
channels, …) and opens its listeners. Idempotent on a daemon that is not
held. A refused release (the config reload failed, the marker could not be
removed, or the work start failed) prints the daemon's reason and exits 1.
After a non-zero exit, decide by the marker file, never by retrying blindly.
Requires an admin key.

Exit codes: 0 released (held: false), 1 on any error.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runDaemonRelease,
}

func init() {
	daemonCmd.AddCommand(daemonReleaseCmd)
}

func runDaemonRelease(cmd *cobra.Command, _ []string) error {
	resp, err := ClientFromEnv().Delete("/api/v1/daemon/hold")
	if err != nil {
		return fmt.Errorf("failed to connect to vornik: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	var out struct {
		Held *bool `json:"held"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Held == nil {
		return fmt.Errorf("failed to parse response: held missing")
	}
	if *out.Held {
		return fmt.Errorf("the daemon answered held: true")
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "held: false")
	return nil
}
