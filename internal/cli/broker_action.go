package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

// `vornikctl broker-action list|resolve` — the operator surface for broker
// write actions (https://docs.vornik.io
// 2026-09-29-broker-write-actions-and-push-design.md §5.4). Approval happens
// in /inbox; this is for the rows no one can decide automatically: an action
// the daemon claimed but could not finish (executing after a restart), or one
// whose call may or may not have reached the vendor (unknown).
//
// resolve NEVER re-executes. The operator checks the vendor (was the email
// sent?) and records what happened.

var (
	brokerActionListProject string
	brokerActionListStatus  string
	brokerActionListJSON    bool

	brokerActionResolveExecuted bool
	brokerActionResolveFailed   bool
	brokerActionResolveNote     string
)

// brokerActionListCap is how many rows list reads per status, oldest first.
const brokerActionListCap = 200

// brokerActionAttentionStatuses are listed when --status is not given: the
// states an operator may need to act on or is waiting for.
var brokerActionAttentionStatuses = []string{
	persistence.BrokerActionPending,
	persistence.BrokerActionApproved,
	persistence.BrokerActionExecuting,
	persistence.BrokerActionUnknown,
}

var brokerActionCmd = &cobra.Command{
	Use:   "broker-action",
	Short: "Operate broker write actions (writes proposed by broker workflows)",
	Long: `Operator surface for broker write actions.

A broker workflow may propose a write (reply to an email, file a ticket). A
person approves it in the web /inbox, and the daemon then calls the tool once
with exactly the approved arguments. Nothing retries a write: when the daemon
cannot tell whether the call reached the vendor, the action is left 'unknown'
for an operator, and 'resolve' records what actually happened.`,
}

var brokerActionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List broker actions awaiting approval, execution or resolution",
	Args:  cobra.NoArgs,
	RunE:  runBrokerActionList,
}

var brokerActionResolveCmd = &cobra.Command{
	Use:   "resolve <action_id>",
	Short: "Record the verified outcome of a stuck broker action",
	Long: `Record the outcome of a broker action the daemon could not confirm.

Only actions in 'executing' (the daemon stopped mid-call) or 'unknown' (the
call may have reached the vendor) are resolvable. Check the vendor, then:

  vornikctl broker-action resolve <id> --executed --note "reply is in Sent"
  vornikctl broker-action resolve <id> --failed   --note "not in Sent"

Exactly one of --executed / --failed is required. This command NEVER calls the
tool again; it records the outcome, attributed to you, as operator_resolved.`,
	Args: cobra.ExactArgs(1),
	RunE: runBrokerActionResolve,
}

func init() {
	brokerActionListCmd.Flags().StringVar(&brokerActionListProject, "project", "", "Only this project")
	brokerActionListCmd.Flags().StringVar(&brokerActionListStatus, "status", "",
		"Only this status (default: pending, approved, executing and unknown)")
	brokerActionListCmd.Flags().BoolVar(&brokerActionListJSON, "json", false, "Print JSON")

	brokerActionResolveCmd.Flags().BoolVar(&brokerActionResolveExecuted, "executed", false,
		"Record that the call did reach the vendor")
	brokerActionResolveCmd.Flags().BoolVar(&brokerActionResolveFailed, "failed", false,
		"Record that nothing was sent")
	brokerActionResolveCmd.Flags().StringVar(&brokerActionResolveNote, "note", "",
		"What you checked (stored with the outcome)")

	brokerActionCmd.AddCommand(brokerActionListCmd, brokerActionResolveCmd)
	rootCmd.AddCommand(brokerActionCmd)
}

func openBrokerActionRepo(ctx context.Context) (persistence.BrokerActionRepository, func(), error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	if backend.Repos == nil || backend.Repos.BrokerActions == nil {
		_ = backend.Close()
		return nil, nil, errors.New("this daemon's storage has no broker-action store")
	}
	return backend.Repos.BrokerActions, func() { _ = backend.Close() }, nil
}

func runBrokerActionList(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openBrokerActionRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()
	return listBrokerActions(ctx, repo, brokerActionListProject, brokerActionListStatus, brokerActionListJSON, cmd.OutOrStdout())
}

// brokerActionListRow is the list output. It carries no arguments and no
// outcome: those are shown in /inbox, where approval is gated.
type brokerActionListRow struct {
	ActionID     string `json:"action_id"`
	ProjectID    string `json:"project_id"`
	TaskID       string `json:"task_id"`
	ActionKind   string `json:"action_kind"`
	Tool         string `json:"tool"`
	Status       string `json:"status"`
	OutcomeClass string `json:"outcome_class,omitempty"`
	Approver     string `json:"approver,omitempty"`
	CreatedAt    string `json:"created_at"`
	ExpiresAt    string `json:"expires_at"`
}

func listBrokerActions(ctx context.Context, repo persistence.BrokerActionRepository, project, status string, asJSON bool, out io.Writer) error {
	statuses := brokerActionAttentionStatuses
	if s := strings.TrimSpace(status); s != "" {
		statuses = []string{s}
	}
	rows := []brokerActionListRow{}
	var capped []string
	for _, s := range statuses {
		got, err := repo.ListByStatus(ctx, project, s, brokerActionListCap)
		if err != nil {
			return fmt.Errorf("list %s broker actions: %w", s, err)
		}
		if len(got) == brokerActionListCap {
			capped = append(capped, s)
		}
		for _, a := range got {
			rows = append(rows, brokerActionListRow{
				ActionID: a.ActionID, ProjectID: a.ProjectID, TaskID: a.TaskID,
				ActionKind: a.ActionKind, Tool: a.Tool, Status: a.Status,
				OutcomeClass: a.OutcomeClass, Approver: a.Approver,
				CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339),
				ExpiresAt: a.ExpiresAt.UTC().Format(time.RFC3339),
			})
		}
	}
	// A full page is disclosed in the table output (review-20260930-1334
	// F3). JSON output stays one document; a caller detects a full page by
	// comparing a status's row count with the cap (200).
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "no broker actions")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ACTION\tPROJECT\tKIND\tTOOL\tSTATUS\tCREATED")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ActionID, r.ProjectID, r.ActionKind, r.Tool, r.Status, r.CreatedAt)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, s := range capped {
		_, _ = fmt.Fprintf(out, "only the oldest %d %s actions are shown; there may be more (use --project to narrow)\n", brokerActionListCap, s)
	}
	return nil
}

func runBrokerActionResolve(cmd *cobra.Command, args []string) error {
	status, err := brokerActionResolveStatus(brokerActionResolveExecuted, brokerActionResolveFailed)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openBrokerActionRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()
	if err := resolveBrokerAction(ctx, repo, args[0], status, currentOperatorIdentity(), brokerActionResolveNote, time.Now().UTC()); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "broker action %s resolved as %s\n", args[0], status)
	return nil
}

// brokerActionResolveStatus maps the two opposite flags; neither or both is
// a usage error, never a guess.
func brokerActionResolveStatus(executed, failed bool) (string, error) {
	switch {
	case executed && failed:
		return "", errors.New("pass exactly one of --executed or --failed, not both")
	case executed:
		return persistence.BrokerActionExecuted, nil
	case failed:
		return persistence.BrokerActionFailed, nil
	default:
		return "", errors.New("one of --executed or --failed is required")
	}
}

// resolveBrokerAction records the operator's outcome. The stored note is
// never empty: the design requires a non-null outcome on every terminal row.
func resolveBrokerAction(ctx context.Context, repo persistence.BrokerActionRepository, actionID, status, operator, note string, now time.Time) error {
	a, err := repo.Get(ctx, actionID)
	if errors.Is(err, persistence.ErrNotFound) {
		return fmt.Errorf("no broker action %s", actionID)
	}
	if err != nil {
		return fmt.Errorf("read broker action %s: %w", actionID, err)
	}
	// An executing row that is not yet stuck may have its call in flight:
	// resolving it --failed would misrecord a write about to be sent
	// (review-20260930-1334 F1). Stuck means older than the longest
	// action timeout, so its call has ended one way or the other.
	if a.Status == persistence.BrokerActionExecuting && !persistence.BrokerActionIsStuck(a, now) {
		return fmt.Errorf("broker action %s started executing %s ago and its call may still be in flight; "+
			"wait until it has been executing for %s (it will then be finished or stuck), nothing was changed",
			actionID, now.Sub(persistence.BrokerActionAgeSince(a)).Truncate(time.Second), persistence.BrokerActionStuckAfter)
	}
	note = strings.TrimSpace(note)
	if note == "" {
		note = "resolved by operator; no note given"
	}
	body, _ := json.Marshal(map[string]string{"note": note, "resolved_by": operator})
	if err := repo.Resolve(ctx, actionID, status, operator, body, now); err != nil {
		if errors.Is(err, persistence.ErrBrokerActionNoTransition) {
			return fmt.Errorf("broker action %s is not resolvable (it must be executing or unknown); nothing was changed", actionID)
		}
		return fmt.Errorf("resolve broker action %s: %w", actionID, err)
	}
	return nil
}
