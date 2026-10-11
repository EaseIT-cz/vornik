package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// defaultDaemonDrainTTL matches the daemon's default lease.
const defaultDaemonDrainTTL = 5 * time.Minute

var daemonDrainTTL = defaultDaemonDrainTTL

// daemonDrainCmd sets the drain bit FIRST, so the deploy gate reads the
// in-flight count after it and no chat request can start uncounted before
// the restart (GitHub #290). Exit 0 drained, 1 any error.
// see LLD § https://docs.vornik.io §9
var daemonDrainCmd = &cobra.Command{
	Use:   "drain",
	Short: "Refuse new chat-proxy requests for a while (before a restart)",
	Long: `Set the daemon's drain bit: new chat-proxy requests get 503 DRAINING with
Retry-After, requests already in flight finish and are still counted by
'vornikctl daemon inflight'. The drain is a lease: it clears itself after
--ttl unless renewed by another 'drain' (idempotent), cleared by
'vornikctl daemon undrain', or made permanent by the restart's SIGTERM.
Used by scripts/deploy-idle-gate.sh --drain. Requires an admin key.

Exit codes: 0 drained, 1 on any error.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runDaemonDrain,
}

// daemonUndrainCmd clears an operator drain (an aborted deploy).
var daemonUndrainCmd = &cobra.Command{
	Use:   "undrain",
	Short: "Clear an operator drain (an aborted deploy)",
	Long: `Clear the drain set by 'vornikctl daemon drain'; chat-proxy requests are
accepted again. Idempotent. Refused (exit 1) once the daemon is shutting
down. Requires an admin key.

Exit codes: 0 cleared, 1 on any error.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runDaemonUndrain,
}

func init() {
	daemonDrainCmd.Flags().DurationVar(&daemonDrainTTL, "ttl", defaultDaemonDrainTTL,
		"how long the drain holds unless renewed (whole seconds, 1s to 1h)")
	daemonCmd.AddCommand(daemonDrainCmd)
	daemonCmd.AddCommand(daemonUndrainCmd)
}

type daemonDrainBody struct {
	Draining     *bool  `json:"draining"`
	ShuttingDown bool   `json:"shutting_down"`
	ExpiresAt    string `json:"expires_at"`
	ChatProxy    int64  `json:"chat_proxy"`
}

func readDrainBody(resp *http.Response) (daemonDrainBody, error) {
	var out daemonDrainBody
	if resp.StatusCode != http.StatusOK {
		return out, ParseAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return out, fmt.Errorf("failed to read response: %w", err)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("failed to parse response: %w", err)
	}
	if out.Draining == nil {
		return out, fmt.Errorf("failed to parse response: draining missing")
	}
	return out, nil
}

func runDaemonDrain(cmd *cobra.Command, _ []string) error {
	ttl := daemonDrainTTL
	if ttl < time.Second || ttl > time.Hour || ttl%time.Second != 0 {
		return fmt.Errorf("--ttl must be whole seconds from 1s to 1h, got %s", ttl)
	}
	resp, err := ClientFromEnv().Post("/api/v1/daemon/drain", map[string]int64{"ttl_seconds": int64(ttl / time.Second)})
	if err != nil {
		return fmt.Errorf("failed to connect to vornik: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := readDrainBody(resp)
	if err != nil {
		return err
	}
	if !*out.Draining {
		return fmt.Errorf("the daemon answered draining: false")
	}
	w := cmd.OutOrStdout()
	if out.ShuttingDown {
		_, _ = fmt.Fprintf(w, "draining: true (shutting down; in-flight chat-proxy requests: %d)\n", out.ChatProxy)
		return nil
	}
	_, _ = fmt.Fprintf(w, "draining: true (expires in %s; in-flight chat-proxy requests: %d)\n", ttl, out.ChatProxy)
	return nil
}

func runDaemonUndrain(cmd *cobra.Command, _ []string) error {
	resp, err := ClientFromEnv().Delete("/api/v1/daemon/drain")
	if err != nil {
		return fmt.Errorf("failed to connect to vornik: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := readDrainBody(resp)
	if err != nil {
		return err
	}
	if *out.Draining {
		return fmt.Errorf("the daemon is still draining")
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "draining: false")
	return nil
}
