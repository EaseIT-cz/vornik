package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

// daemonCmd groups reads about the running daemon process itself.
var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Inspect the running daemon process",
}

var daemonInflightJSON bool

// daemonInflightCmd reads the in-flight long-request count for the deploy
// idle gate (GitHub #97). Exit codes are the contract the gate relies on:
// 0 idle, 2 busy, 1 any error, so a failed read is never read as idle.
// see LLD § https://docs.vornik.io §8
var daemonInflightCmd = &cobra.Command{
	Use:   "inflight",
	Short: "Show in-flight chat-proxy requests; exit 2 when busy",
	Long: `Show how many chat-proxy requests the daemon is serving right now, and
whether a drain has started. Used by scripts/deploy-idle-gate.sh before a
restart. Requires an admin key.

Exit codes: 0 when nothing is in flight, 2 when something is, 1 on any error.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runDaemonInflight,
}

func init() {
	daemonInflightCmd.Flags().BoolVar(&daemonInflightJSON, "json", false, "print the JSON body")
	daemonCmd.AddCommand(daemonInflightCmd)
	rootCmd.AddCommand(daemonCmd)
}

type daemonInflight struct {
	ChatProxy *int64 `json:"chat_proxy"`
	Total     *int64 `json:"total"`
	Draining  bool   `json:"draining"`
}

// errDaemonBusy is exit code 2 with nothing more printed: the count is on
// stdout already.
var errDaemonBusy = &exitCodeError{code: 2}

func runDaemonInflight(cmd *cobra.Command, _ []string) error {
	resp, err := ClientFromEnv().Get("/api/v1/daemon/inflight")
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
	var out daemonInflight
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}
	if out.Total == nil || out.ChatProxy == nil {
		return fmt.Errorf("failed to parse response: total or chat_proxy missing")
	}
	w := cmd.OutOrStdout()
	if daemonInflightJSON {
		_, _ = fmt.Fprintf(w, "%s\n", body)
	} else {
		_, _ = fmt.Fprintf(w, "in-flight chat-proxy requests: %d (draining: %t)\n", *out.ChatProxy, out.Draining)
	}
	if *out.Total > 0 {
		return errDaemonBusy
	}
	return nil
}
