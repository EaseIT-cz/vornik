package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/leaderelection"
)

// `vornikctl leader-lock release` (issue #60; horizontal scaling LLD,
// implementation contract 2026-09-25). The daemon decides every outcome —
// the expiry predicate in its one DeleteExpired statement is the control — and
// this command prints the answer and turns it into an exit code a script can
// act on: 0 released, 1 failed, 2 usage, 3 re-run, 4 live holder, 5 unknown
// id (precedence in leaderelection.ReleaseExitCode). There is no --force.

var (
	leaderLockAllOrphaned bool
	leaderLockReason      string
	leaderLockJSON        bool
)

var leaderLockCmd = &cobra.Command{
	Use:   "leader-lock",
	Short: "Inspect and release daemon leader locks",
}

var leaderLockReleaseCmd = &cobra.Command{
	Use:   "release [worker-id...]",
	Short: "Release EXPIRED leader-lock rows (refuses a live or stale lease)",
	Long: `Release leader-lock rows whose lease has expired, so the doctor stops
reporting a lock no elector will renew (a disabled feature, a deleted project,
an Enterprise worker on a Community build).

Only an EXPIRED row is ever released, decided by the daemon in one statement.
A stale or live row is refused with the reason; there is no --force, because a
delete cannot evict a live holder (it re-acquires within a heartbeat) and would
reset the epoch fence. To stop a live holder, stop its process.

Exit codes: 0 all released, 1 the call failed, 2 usage, 3 refused but a re-run
will fix it, 4 a live holder, 5 no such worker id.`,
	SilenceUsage: true,
	// main prints a non-empty error once; cobra printing it too gave a bare
	// "Error: " line for the silent exit codes and every flag error twice
	// (seen on the first deploy, 2026-09-25).
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLeaderLockRelease(args, leaderLockAllOrphaned, leaderLockReason, leaderLockJSON, cmd.OutOrStdout())
	},
}

func init() {
	leaderLockReleaseCmd.Flags().BoolVar(&leaderLockAllOrphaned, "all-orphaned", false, "release every expired row no elector in this daemon wires (requires --reason)")
	leaderLockReleaseCmd.Flags().StringVar(&leaderLockReason, "reason", "", "why, recorded in the admin audit log")
	leaderLockReleaseCmd.Flags().BoolVar(&leaderLockJSON, "json", false, "print the daemon's raw response")
	// An unknown flag (--force included) is a usage error, exit 2, never
	// silently ignored.
	leaderLockReleaseCmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitCodeError{code: leaderelection.ExitUsage, msg: err.Error()}
	})
	leaderLockCmd.AddCommand(leaderLockReleaseCmd)
	rootCmd.AddCommand(leaderLockCmd)
}

type leaderLockReleaseResultRow struct {
	WorkerID   string `json:"worker_id"`
	Outcome    string `json:"outcome"`
	Message    string `json:"message"`
	AuditError string `json:"audit_error,omitempty"`
}

type leaderLockReleaseAnswer struct {
	Results        []leaderLockReleaseResultRow `json:"results"`
	KnownWorkerIDs []string                     `json:"known_worker_ids,omitempty"`
}

func runLeaderLockRelease(args []string, allOrphaned bool, reason string, jsonOut bool, out io.Writer) error {
	usage := func(msg string) error { return &exitCodeError{code: leaderelection.ExitUsage, msg: msg} }
	switch {
	case len(args) == 0 && !allOrphaned:
		return usage("name at least one worker id, or pass --all-orphaned")
	case len(args) > 0 && allOrphaned:
		return usage("give worker ids or --all-orphaned, not both")
	case allOrphaned && reason == "":
		return usage("--all-orphaned requires --reason")
	}

	body := map[string]any{"reason": reason}
	if allOrphaned {
		body["all_orphaned"] = true
	} else {
		body["worker_ids"] = args
	}
	resp, err := ClientFromEnv().Post("/api/v1/admin/leader-locks/release", body)
	if err != nil {
		return &exitCodeError{code: leaderelection.ExitError, msg: fmt.Sprintf("leader-lock release: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		code := leaderelection.ExitError
		if resp.StatusCode == http.StatusBadRequest {
			code = leaderelection.ExitUsage
		}
		return &exitCodeError{code: code, msg: ParseAPIError(resp).Error()}
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return &exitCodeError{code: leaderelection.ExitError, msg: fmt.Sprintf("leader-lock release: read: %v", err)}
	}
	var ans leaderLockReleaseAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return &exitCodeError{code: leaderelection.ExitError, msg: fmt.Sprintf("leader-lock release: decode: %v", err)}
	}
	if jsonOut {
		_, _ = out.Write(raw)
	} else {
		printLeaderLockRelease(out, ans, allOrphaned)
	}

	outcomes := make([]string, 0, len(ans.Results))
	auditFailed := false
	for _, r := range ans.Results {
		outcomes = append(outcomes, r.Outcome)
		// A released row whose audit write failed is released, but the
		// operation is not complete without its record: exit as a failure.
		auditFailed = auditFailed || r.AuditError != ""
	}
	if code := leaderelection.ReleaseExitCode(outcomes, auditFailed); code != leaderelection.ExitReleased {
		return &exitCodeError{code: code}
	}
	return nil
}

func printLeaderLockRelease(out io.Writer, ans leaderLockReleaseAnswer, allOrphaned bool) {
	if allOrphaned && len(ans.Results) == 0 {
		_, _ = fmt.Fprintln(out, "0 orphaned rows found; nothing released")
		return
	}
	released := 0
	for _, r := range ans.Results {
		if r.Outcome == leaderelection.OutcomeReleased {
			released++
		}
		_, _ = fmt.Fprintf(out, "%-16s %s  %s\n", r.Outcome, r.WorkerID, r.Message)
		if r.AuditError != "" {
			_, _ = fmt.Fprintf(out, "%-16s %s  AUDIT WRITE FAILED: %s\n", "", r.WorkerID, r.AuditError)
		}
	}
	if len(ans.KnownWorkerIDs) > 0 {
		_, _ = fmt.Fprintf(out, "known worker ids: %v\n", ans.KnownWorkerIDs)
	}
	_, _ = fmt.Fprintf(out, "%d released, %d refused\n", released, len(ans.Results)-released)
}

// exitCodeError carries a process exit code. main exits with ExitCodeOf(err);
// an empty msg prints nothing, for an outcome the command already reported.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

// ExitCode is the process exit code this error asks for.
func (e *exitCodeError) ExitCode() int { return e.code }

// ExitCode is featureExitError's code, which main used to ignore.
func (e *featureExitError) ExitCode() int { return e.code }

// ExitCodeOf is the process exit code for a command's error: 0 for nil, the
// error's own ExitCode() when it has one, 1 otherwise. vornikctl exited 1 for
// every error until 2026-09-25, so no command could return anything else.
func ExitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return 1
}
