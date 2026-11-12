package cli

// vornikctl credential (GitHub #625 item 2; agent-administered design, #623
// follow-up): list, set and remove operator credentials through the admin API
// with an admin-class key (VORNIK_API_KEY). A value is read from standard
// input only, so it never reaches an argv or shell history.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

const credentialAPIPath = "/api/v1/admin/operator-credentials"

var credentialCmd = &cobra.Command{
	Use:   "credential",
	Short: "List, set and remove operator credentials (the console's Credentials page, from a shell)",
	Long: `Operator credentials are values stored encrypted in the daemon's credential
store and referenced from config as secret://NAME. These verbs call the admin
API with an admin-class key (VORNIK_API_KEY); every write is audited and never
records the value. On a managed instance, use the operations address.`,
}

var credentialListCmd = &cobra.Command{
	Use:   "list",
	Short: "List operator credentials and the names the config needs (never a value)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		resp, err := ClientFromEnv().Get(credentialAPIPath)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return ParseAPIError(resp)
		}
		var list struct {
			KeyUnusable bool `json:"key_unusable"`
			Credentials []struct {
				Name     string   `json:"name"`
				State    string   `json:"state"`
				SetAt    string   `json:"set_at"`
				NeededBy []string `json:"needed_by"`
			} `json:"credentials"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		return renderCredentialList(cmd.OutOrStdout(), list.KeyUnusable, func(w io.Writer) {
			for _, c := range list.Credentials {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Name, c.State, dashIfEmpty(c.SetAt), dashIfEmpty(strings.Join(c.NeededBy, ", ")))
			}
		})
	},
}

func renderCredentialList(out io.Writer, keyUnusable bool, rows func(io.Writer)) error {
	if keyUnusable {
		_, _ = fmt.Fprintln(out, "The credential store key is missing or wrong: stored credentials cannot be read (vornikctl doctor, secret_store_key).")
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSTATE\tSET AT\tNEEDED BY")
	rows(tw)
	return tw.Flush()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var credentialSetCmd = &cobra.Command{
	Use:   "set NAME",
	Short: "Store or replace an operator credential, the value read from standard input",
	Long: `Reads the value from standard input (one trailing newline is stripped):

  printf '%s' "$TOKEN" | vornikctl credential set CONTEXT7_API_KEY

It refuses a terminal on standard input, so the value is never typed where it
is echoed, and it never appears in an argument.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		value, err := readCredentialValue(cmd.InOrStdin())
		if err != nil {
			return err
		}
		resp, err := ClientFromEnv().DoRaw(http.MethodPut, credentialAPIPath+"/"+args[0], value, "application/octet-stream")
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return ParseAPIError(resp)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "stored %s\n", args[0])
		return nil
	},
}

var credentialRemoveCmd = &cobra.Command{
	Use:   "remove NAME",
	Short: "Remove an operator credential from the store",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := ClientFromEnv().Do(http.MethodDelete, credentialAPIPath+"/"+args[0], nil)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return ParseAPIError(resp)
		}
		var out struct {
			Removed bool `json:"removed"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return fmt.Errorf("the daemon answered, but the reply could not be read: %w", err)
		}
		if out.Removed {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", args[0])
		} else {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s was not in the store (it may still come from the environment)\n", args[0])
		}
		return nil
	},
}

// readCredentialValue reads the value from standard input, refusing a
// terminal, and strips one trailing newline.
func readCredentialValue(in io.Reader) ([]byte, error) {
	if stdinIsTerminal(in) {
		return nil, errors.New("standard input is a terminal: pipe the value in (printf '%s' \"$VALUE\" | vornikctl credential set NAME)")
	}
	v, err := io.ReadAll(io.LimitReader(in, 16<<10+1))
	if err != nil {
		return nil, err
	}
	v = bytes.TrimSuffix(v, []byte("\n"))
	if len(v) == 0 {
		return nil, errors.New("no value on standard input")
	}
	return v, nil
}

func init() {
	credentialCmd.AddCommand(credentialListCmd, credentialSetCmd, credentialRemoveCmd)
	rootCmd.AddCommand(credentialCmd)
}
