package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// vornikctl companion hints (GitHub #634 slice 2; companion hardening design
// §13 H6, §13.10 S6): each catalogue hint's state, window and counts for a
// project, and when each companion key last fetched and uploaded.

var (
	companionHintsProject string
	companionHintsJSON    bool
)

var companionHintsCmd = &cobra.Command{
	Use:   "hints",
	Short: "List the companion hints for a project: state, follow rate, last client contact",
	Long: `List each companion hint for a project: whether it is active, muted,
inactive (shipped muted), backed off or off, its last 10 firings and follow
rate, and when each companion key last fetched the catalogue and uploaded
outcomes. Mute a hint or turn hints off in the project's companion block.`,
	RunE: runCompanionHints,
}

func init() {
	companionHintsCmd.Flags().StringVarP(&companionHintsProject, "project", "p", "", "Project ID (required)")
	companionHintsCmd.Flags().BoolVar(&companionHintsJSON, "json", false, "Emit the report as JSON")
	_ = companionHintsCmd.MarkFlagRequired("project")
	companionCmd.AddCommand(companionHintsCmd)
}

type hintReportOut struct {
	Project string `json:"project"`
	Enabled bool   `json:"enabled"`
	Hints   []struct {
		ID            string     `json:"id"`
		Trigger       string     `json:"trigger"`
		State         string     `json:"state"`
		BackoffUntil  *time.Time `json:"backoff_until"`
		Window        string     `json:"window"`
		FollowRate    *float64   `json:"follow_rate"`
		FiredTotal    int        `json:"fired_total"`
		FollowedTotal int        `json:"followed_total"`
	} `json:"hints"`
	Clients []struct {
		KeyID        string     `json:"key_id"`
		LastFetchAt  *time.Time `json:"last_fetch_at"`
		LastUploadAt *time.Time `json:"last_upload_at"`
	} `json:"clients"`
}

func runCompanionHints(cmd *cobra.Command, _ []string) error {
	resp, err := ClientFromEnv().Get("/api/v1/companion/hints/report?projectId=" + url.QueryEscape(companionHintsProject))
	if err != nil {
		return fmt.Errorf("hints: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("hints: %w", err)
	}
	out := cmd.OutOrStdout()
	if companionHintsJSON {
		_, err := fmt.Fprintln(out, strings.TrimSpace(string(raw)))
		return err
	}
	var rep hintReportOut
	if err := json.Unmarshal(raw, &rep); err != nil {
		return fmt.Errorf("hints: decode: %w", err)
	}
	return printHintReport(out, rep)
}

// stickyWriter keeps the first write error, so a report is printed in plain
// steps and checked once.
type stickyWriter struct {
	w   io.Writer
	err error
}

func (s *stickyWriter) printf(format string, a ...any) {
	if s.err == nil {
		_, s.err = fmt.Fprintf(s.w, format, a...)
	}
}

func printHintReport(out io.Writer, rep hintReportOut) error {
	p := &stickyWriter{w: out}
	if !rep.Enabled {
		p.printf("Hints are off for project %q (companion.hints: off).\n\n", rep.Project)
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	t := &stickyWriter{w: tw}
	t.printf("HINT\tTRIGGER\tSTATE\tLAST 10\tRATE\tFIRED\tFOLLOWED\n")
	for _, h := range rep.Hints {
		state := strings.ReplaceAll(h.State, "_", " ")
		if h.BackoffUntil != nil {
			state += " until " + h.BackoffUntil.UTC().Format(time.DateOnly)
		}
		last, rate := "-", "-"
		if n := len(h.Window); n > 0 {
			last = fmt.Sprintf("%d/%d", strings.Count(h.Window, "1"), n)
		}
		if h.FollowRate != nil {
			rate = fmt.Sprintf("%.0f%%", *h.FollowRate*100)
		}
		t.printf("%s\t%s\t%s\t%s\t%s\t%d\t%d\n", h.ID, h.Trigger, state, last, rate, h.FiredTotal, h.FollowedTotal)
	}
	if t.err != nil {
		return t.err
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	p.printf("\n")
	if len(rep.Clients) == 0 {
		p.printf("No companion key has fetched the hints yet; clients run on their embedded copy.\n")
		return p.err
	}
	tw = tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	t = &stickyWriter{w: tw}
	t.printf("KEY\tLAST FETCH\tLAST UPLOAD\n")
	when := func(at *time.Time) string {
		if at == nil {
			return "never"
		}
		return at.UTC().Format(time.RFC3339)
	}
	for _, c := range rep.Clients {
		t.printf("%s\t%s\t%s\n", c.KeyID, when(c.LastFetchAt), when(c.LastUploadAt))
	}
	if t.err != nil {
		return t.err
	}
	if p.err != nil {
		return p.err
	}
	return tw.Flush()
}
