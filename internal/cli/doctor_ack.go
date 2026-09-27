package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

// doctorAckCmd records that an operator declines what a doctor check reports
// for one deployed config file (config-tree drift design, slice C). The DAEMON
// writes the acknowledgement, so it lands in the tree the daemon resolved —
// the one config_template_drift reads — not wherever this CLI would resolve
// it. Admin key required.
var doctorAckCmd = &cobra.Command{
	Use:   "ack <check> <file>",
	Short: "Acknowledge a config_template_drift finding you have decided not to take",
	Long: `Record that you decline what config_template_drift currently reports for one
deployed config file, e.g.

  vornikctl doctor ack config_template_drift workflows/dev-pipeline.md

The acknowledgement covers that file's findings AS THEY ARE NOW: a later
template change, a new removal or an edit to a canonical override re-opens the
row, because it is a new decision. Requires an admin key. Only
config_template_drift is acknowledgeable.`,
	Args: cobra.ExactArgs(2),
	// A refusal comes from the daemon (no baseline, nothing to acknowledge);
	// the usage text would read as if the command were mistyped.
	SilenceUsage: true,
	RunE:         runDoctorAck,
}

func init() {
	doctorCmd.AddCommand(doctorAckCmd)
}

type doctorAckResponse struct {
	File     string `json:"file"`
	Recorded []struct {
		Class, Key, Regime string
	} `json:"recorded"`
	Pruned []struct {
		Rel, Class, Key, Regime string
	} `json:"pruned"`
	Date string `json:"date"`
}

func runDoctorAck(cmd *cobra.Command, args []string) error {
	resp, err := ClientFromEnv().Post("/api/v1/doctor/ack", map[string]string{"check": args[0], "file": args[1]})
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
	var out doctorAckResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "acknowledged %d finding(s) for %s (%s)\n", len(out.Recorded), out.File, out.Date)
	for _, r := range out.Recorded {
		_, _ = fmt.Fprintf(w, "  %-9s %s\n", r.Class, ackKeyPrefix(r.Key))
	}
	if len(out.Pruned) > 0 {
		_, _ = fmt.Fprintf(w, "pruned %d stale acknowledgement(s) whose finding no longer exists:\n", len(out.Pruned))
		for _, r := range out.Pruned {
			_, _ = fmt.Fprintf(w, "  %s %s %s\n", r.Rel, r.Class, ackKeyPrefix(r.Key))
		}
	}
	return nil
}

func ackKeyPrefix(k string) string {
	if len(k) > 16 {
		return k[:16]
	}
	return k
}
