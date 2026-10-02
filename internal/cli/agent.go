package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/agentbridge"
)

// `vornikctl agent ...` (agent-administered Vornik design §11, plan P6).
// A harness reaches Vornik through a stdio MCP server entry that runs
// `vornikctl agent mcp-bridge --namespace <ns>`. The admin key lives in one
// file only the bridge reads, so no harness config holds it.

var (
	agentNamespace string
	agentURL       string
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Connect an AI assistant (Hermes, Claude, Codex) to Vornik",
}

var agentMCPBridgeCmd = &cobra.Command{
	Use:   "mcp-bridge",
	Short: "Relay MCP over stdio to Vornik (run by the assistant, not by you)",
	Long: `Relay newline-delimited MCP JSON-RPC between stdin/stdout and Vornik's
companion endpoint, authenticated with the namespace's key file
(<config dir>/vornik/agents/<namespace>.key, written by vornikctl agent connect).

The assistant runs this command from its MCP server entry. stdout carries the
protocol only; diagnostics go to stderr and never include the key.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runMCPBridge(ctx, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

func init() {
	agentMCPBridgeCmd.Flags().StringVar(&agentNamespace, "namespace", "", "The agent namespace whose key to use (required)")
	agentMCPBridgeCmd.Flags().StringVar(&agentURL, "url", "", "Vornik's URL (default $VORNIK_API_URL, then "+DefaultAPIURL+")")
	_ = agentMCPBridgeCmd.MarkFlagRequired("namespace")
	agentCmd.AddCommand(agentMCPBridgeCmd)
	rootCmd.AddCommand(agentCmd)
}

// agentBaseURL is --url, then VORNIK_API_URL, then the default: the order
// every other vornikctl command uses.
func agentBaseURL() string {
	for _, u := range []string{agentURL, os.Getenv("VORNIK_API_URL")} {
		if u = strings.TrimSpace(u); u != "" {
			return strings.TrimRight(u, "/")
		}
	}
	return DefaultAPIURL
}

func runMCPBridge(ctx context.Context, in io.Reader, out, errw io.Writer) error {
	base := agentBaseURL()
	if err := agentbridge.CheckEndpoint(base); err != nil {
		return err
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("no config directory: %w", err)
	}
	path, err := agentbridge.KeyPath(dir, agentNamespace)
	if err != nil {
		return err
	}
	key, err := agentbridge.LoadKey(path)
	if err != nil {
		return err
	}
	return agentbridge.Run(ctx, in, out, errw, agentbridge.Config{Endpoint: base + "/api/v1/mcp/companion", Key: key})
}
