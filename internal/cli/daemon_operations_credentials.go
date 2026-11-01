package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
)

// daemonOpsCredsConfig is --config: the managed instance's config file.
var daemonOpsCredsConfig string

// daemonOpsCredsCmd prints the operations listener's URL and the instance's
// current and previous instance_admin keys, read from the config file as
// written (GitHub #581 D3, enterprise-packaging-design.md amendment
// 2026-10-29). The managed idle gate reads them into the environment of its
// own vornikctl calls, so no shell parses YAML and no key reaches an argv. It
// prints to a pipe only, never to a terminal.
var daemonOpsCredsCmd = &cobra.Command{
	Use:   "operations-credentials",
	Short: "Print the managed operations URL and admin keys, for the idle gate",
	Long: `Print, from a managed instance's config file, the operations listener's
URL and its current and previous instance_admin keys, one per line:

  url=http://<hosting.operations_address>
  key=<the current key>
  previous=<hosting.previous_admin_api_key, or empty>

For /usr/libexec/vornik-enterprise/deploy-idle-gate, run as root. It refuses
to print to a terminal, a self-hosted config, and a file whose current key is
not exactly one literal key listed in both api.api_keys and
admin.allowed_keys.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runDaemonOperationsCredentials(cmd, daemonOpsCredsConfig)
	},
}

func init() {
	daemonOpsCredsCmd.Flags().StringVar(&daemonOpsCredsConfig, "config", "/etc/vornik/vornik.yaml", "the managed instance's config file")
	daemonCmd.AddCommand(daemonOpsCredsCmd)
}

// stdoutIsTerminal reports whether w is a terminal; a seam for the tests.
var stdoutIsTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func runDaemonOperationsCredentials(cmd *cobra.Command, path string) error {
	w := cmd.OutOrStdout()
	if stdoutIsTerminal(w) {
		return errors.New("operations-credentials prints secrets; it writes to a pipe only, not a terminal")
	}
	content, err := os.ReadFile(path) // #nosec G304 -- the operator names the config file
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if config.GetYAMLString(content, "hosting.mode") != config.HostingModeManaged {
		return fmt.Errorf("%s is not a managed instance's config (hosting.mode is not %q)", path, config.HostingModeManaged)
	}
	addr := config.GetYAMLString(content, "hosting.operations_address")
	if addr == "" {
		return fmt.Errorf("%s sets no hosting.operations_address", path)
	}
	keys := config.CurrentAdminKeysInFile(content)
	if len(keys) != 1 {
		return fmt.Errorf("%s holds %d current instance_admin keys (literal in both api.api_keys and admin.allowed_keys, not the previous one); want exactly 1", path, len(keys))
	}
	prev := config.GetYAMLString(content, "hosting.previous_admin_api_key")
	_, err = fmt.Fprintf(w, "url=http://%s\nkey=%s\nprevious=%s\n", addr, keys[0], prev)
	return err
}
