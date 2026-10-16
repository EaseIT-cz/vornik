package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/persistence"
)

// Task responses from the API
type taskResponse struct {
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	ProjectID string `json:"projectId"`
	TaskType  string `json:"taskType"`
	Priority  int    `json:"priority"`
	CreatedAt string `json:"createdAt"`
	LastError string `json:"lastError,omitempty"`
	// ParentTaskID is the delegating parent; empty for a top-level task or
	// from a daemon older than GitHub #408.
	ParentTaskID string `json:"parentTaskId,omitempty"`
}

type listTasksResponse struct {
	Tasks []taskResponse `json:"tasks"`
	Total int            `json:"total"`
	// TotalUnknown: the daemon's count failed and Total is only the page
	// length (GitHub #408); never read it as the whole set.
	TotalUnknown bool `json:"totalUnknown,omitempty"`
}

// taskListOutput is `task list --json`. Tasks and Total keep their old
// meaning; Shown, Truncated and NextOffset state the cut (GitHub #408):
// NextOffset, present only when truncated, is the --offset that continues
// the list.
type taskListOutput struct {
	Tasks      []taskResponse `json:"tasks"`
	Total      int            `json:"total"`
	Shown      int            `json:"shown"`
	Truncated  bool           `json:"truncated"`
	NextOffset *int           `json:"nextOffset,omitempty"`
	// TotalUnknown: the daemon could not count the matches, so Total is a
	// lower bound and Truncated says only whether the walk stopped early.
	TotalUnknown bool `json:"totalUnknown,omitempty"`
}

type cancelTaskResponse struct {
	TaskID      string `json:"taskId"`
	Status      string `json:"status"`
	WasRunning  bool   `json:"wasRunning"`
	CancelledAt string `json:"cancelledAt"`
}

type retryTaskResponse struct {
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	Attempt   int    `json:"attempt"`
	RetriedAt string `json:"retriedAt"`
}

var taskCmd = &cobra.Command{
	Use:   "task",
	Short: "Manage tasks",
	Long:  "Inspect, list, cancel, and retry tasks in the vornik control plane.",
}

var taskListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tasks",
	Long: `List tasks for a project, newest first, optionally filtered by status.

Shows 20 tasks unless --limit or --all says otherwise. When more tasks match
than are shown, the last line says so: "Showing 20 of 72 tasks; pass --all
(or --limit N) to see the rest." --active lists every unfinished task
(PENDING, QUEUED, LEASED, RUNNING, WAITING_FOR_CHILDREN, AWAITING_INPUT,
AWAITING_EXTERNAL, PAUSED, AWAITING_APPROVAL), including parents waiting on
their children. The PARENT column names the task a child was delegated from.

Examples:
  vornikctl task list -p my-project --active --all
  vornikctl task list -p my-project --status QUEUED,WAITING_FOR_CHILDREN
  vornikctl task list -p my-project --limit 100 --json`,
	RunE: runTaskList,
}

var taskCancelCmd = &cobra.Command{
	Use:   "cancel <taskId>",
	Short: "Cancel a task",
	Long: `Cancel a task by ID. Tasks in PENDING, QUEUED, LEASED, RUNNING,
WAITING_FOR_CHILDREN, AWAITING_INPUT, AWAITING_EXTERNAL or PAUSED status can be
cancelled.

Cancelling a parent also cancels its descendants (children, their children,
and so on) that are PENDING, QUEUED, LEASED, RUNNING or WAITING_FOR_CHILDREN,
and stops their running containers. A descendant that is PAUSED or awaiting
input, an external event or approval is not cancelled by the cascade; list
them with 'vornikctl task list --active --all' and cancel them by ID.`,
	Args: cobra.ExactArgs(1),
	RunE: runTaskCancel,
}

var taskRetryCmd = &cobra.Command{
	Use:   "retry <taskId>",
	Short: "Retry a task",
	Long:  "Retry a failed or cancelled task by ID. The task is re-queued for execution.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskRetry,
}

var taskTailCmd = &cobra.Command{
	Use:   "tail <taskId>",
	Short: "Tail task logs",
	Long:  "Print current container logs for a running task, or the latest persisted failure/result excerpt after completion.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskTail,
}

var taskSubmitCmd = &cobra.Command{
	Use:   "submit",
	Short: "Submit a new task",
	Long: `Submit a new task to a project's queue. The --prompt flag is the
operator-friendly shortcut for the context.prompt payload shape every
researcher role already reads; for custom shapes use --context-json.

Examples:
  vornikctl task submit -p family --prompt "Summarise yesterday's scans"
  vornikctl task submit -p snake --task-type feature --prompt "Implement X"
  vornikctl task submit -p n8n-agents --workflow adaptive --prompt "..." --priority 10`,
	RunE: runTaskSubmit,
}

var taskGetCmd = &cobra.Command{
	Use:   "get <taskId>",
	Short: "Get a single task's details",
	Long:  "Fetch one task by ID. Prints the task envelope (status, workflow, timestamps) and, with --json, the full API response including payload.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskGet,
}

// tailCmd is a top-level alias for `vornikctl task tail`. Both share the
// same RunE so behaviour stays identical; keeping the alias avoids
// breaking scripts that pre-date the move into the task subtree.
var tailCmd = &cobra.Command{
	Use:   "tail <taskId>",
	Short: "Tail task logs (alias for `vornikctl task tail`)",
	Long:  "Alias for `vornikctl task tail`. Prints current container logs for a running task, or the latest persisted failure/result excerpt after completion.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskTail,
}

var (
	taskListProject    string
	taskListStatus     string
	taskListJSON       bool
	taskListLimit      int
	taskListOffset     int
	taskListAll        bool
	taskListActive     bool
	taskCancelProject  string
	taskRetryProject   string
	taskRetryResetFlag bool
	taskTailProject    string
	taskTailLines      int
	taskTailFollow     bool

	taskSubmitProject        string
	taskSubmitWorkflow       string
	taskSubmitTaskType       string
	taskSubmitPriority       int
	taskSubmitPrompt         string
	taskSubmitContextJSON    string
	taskSubmitIdempotencyKey string
	taskSubmitJSON           bool
	taskSubmitAttach         []string // file paths to attach via inputArtifacts

	taskGetProject string
	taskGetJSON    bool
)

func init() {
	// task list flags
	taskListCmd.Flags().StringVarP(&taskListProject, "project", "p", "", "Project ID (required)")
	taskListCmd.Flags().StringVarP(&taskListStatus, "status", "s", "", "Filter by status; a comma-separated list matches any (PENDING, QUEUED, LEASED, RUNNING, WAITING_FOR_CHILDREN, AWAITING_APPROVAL, AWAITING_INPUT, AWAITING_EXTERNAL, PAUSED, COMPLETED, FAILED, CANCELLED, CLOSED)")
	taskListCmd.Flags().BoolVar(&taskListJSON, "json", false, "Output in JSON format")
	taskListCmd.Flags().IntVar(&taskListLimit, "limit", taskListDefaultLimit, "Show at most N tasks")
	taskListCmd.Flags().IntVar(&taskListOffset, "offset", 0, "Skip the first N matching tasks (the nextOffset of a --json run continues it)")
	taskListCmd.Flags().BoolVar(&taskListAll, "all", false, "Show every matching task (pages through the API)")
	taskListCmd.Flags().BoolVar(&taskListActive, "active", false, "Every unfinished status, including parents in WAITING_FOR_CHILDREN (not with --status)")
	_ = taskListCmd.MarkFlagRequired("project")

	// task cancel flags
	taskCancelCmd.Flags().StringVarP(&taskCancelProject, "project", "p", "", "Project ID (required)")
	_ = taskCancelCmd.MarkFlagRequired("project")

	// task retry flags
	taskRetryCmd.Flags().StringVarP(&taskRetryProject, "project", "p", "", "Project ID (required)")
	taskRetryCmd.Flags().BoolVar(&taskRetryResetFlag, "reset-attempts", false, "Reset attempt counter to 1")
	_ = taskRetryCmd.MarkFlagRequired("project")

	taskTailCmd.Flags().StringVarP(&taskTailProject, "project", "p", "", "Project ID (required)")
	taskTailCmd.Flags().IntVarP(&taskTailLines, "lines", "n", 200, "Number of lines to show")
	taskTailCmd.Flags().BoolVarP(&taskTailFollow, "follow", "f", false, "Poll and print appended log lines")
	_ = taskTailCmd.MarkFlagRequired("project")
	tailCmd.Flags().StringVarP(&taskTailProject, "project", "p", "", "Project ID (required)")
	tailCmd.Flags().IntVarP(&taskTailLines, "lines", "n", 200, "Number of lines to show")
	tailCmd.Flags().BoolVarP(&taskTailFollow, "follow", "f", false, "Poll and print appended log lines")
	_ = tailCmd.MarkFlagRequired("project")

	// task submit flags
	taskSubmitCmd.Flags().StringVarP(&taskSubmitProject, "project", "p", "", "Project ID (required)")
	taskSubmitCmd.Flags().StringVar(&taskSubmitWorkflow, "workflow", "", "Override the project's default workflow")
	taskSubmitCmd.Flags().StringVar(&taskSubmitTaskType, "task-type", "research", "Task type label (free-form; surfaces in `task list`)")
	taskSubmitCmd.Flags().IntVar(&taskSubmitPriority, "priority", 0, "Priority 0-100; 0 uses the project's default")
	taskSubmitCmd.Flags().StringVar(&taskSubmitPrompt, "prompt", "", "Task prompt — shorthand for --context-json '{\"prompt\":\"...\"}'")
	taskSubmitCmd.Flags().StringVar(&taskSubmitContextJSON, "context-json", "", "Raw JSON object to set as the task context (mutually exclusive with --prompt)")
	taskSubmitCmd.Flags().StringVar(&taskSubmitIdempotencyKey, "idempotency-key", "", "Optional idempotency key; duplicate submits return the existing task")
	taskSubmitCmd.Flags().StringSliceVar(&taskSubmitAttach, "attach", nil, "Attach a file as an input artifact (snapshotted + auto-extracted into project memory). Repeatable; e.g. --attach book.epub --attach paper.pdf")
	taskSubmitCmd.Flags().BoolVar(&taskSubmitJSON, "json", false, "Output the raw API response instead of the human summary")
	_ = taskSubmitCmd.MarkFlagRequired("project")

	// task get flags
	taskGetCmd.Flags().StringVarP(&taskGetProject, "project", "p", "", "Project ID (required)")
	taskGetCmd.Flags().BoolVar(&taskGetJSON, "json", false, "Output in JSON format")
	_ = taskGetCmd.MarkFlagRequired("project")

	// Add subcommands
	taskCmd.AddCommand(taskListCmd)
	taskCmd.AddCommand(taskCancelCmd)
	taskCmd.AddCommand(taskRetryCmd)
	taskCmd.AddCommand(taskTailCmd)
	taskCmd.AddCommand(taskSubmitCmd)
	taskCmd.AddCommand(taskGetCmd)

	// Add to root
	rootCmd.AddCommand(taskCmd)
	rootCmd.AddCommand(tailCmd)
}

// runTaskSubmit wraps POST /api/v1/projects/{p}/tasks with an operator-
// friendly flag surface. --prompt is the common case and gets wrapped
// into the canonical {prompt: ...} context shape; --context-json lets
// power users send any shape the agent runtime understands.
func runTaskSubmit(cmd *cobra.Command, args []string) error {
	if taskSubmitPrompt != "" && taskSubmitContextJSON != "" {
		return fmt.Errorf("--prompt and --context-json are mutually exclusive")
	}

	type attachedArtifact struct {
		Name    string `json:"name"`
		Content string `json:"content"` // base64-encoded
	}
	type submitReq struct {
		TaskType       string             `json:"taskType"`
		Priority       int                `json:"priority,omitempty"`
		WorkflowID     string             `json:"workflowId,omitempty"`
		IdempotencyKey string             `json:"idempotencyKey,omitempty"`
		InputArtifacts []attachedArtifact `json:"inputArtifacts,omitempty"`
		Context        json.RawMessage    `json:"context,omitempty"`
	}
	req := submitReq{
		TaskType:       taskSubmitTaskType,
		Priority:       taskSubmitPriority,
		WorkflowID:     taskSubmitWorkflow,
		IdempotencyKey: taskSubmitIdempotencyKey,
	}
	for _, p := range taskSubmitAttach {
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read --attach %q: %w", p, err)
		}
		req.InputArtifacts = append(req.InputArtifacts, attachedArtifact{
			Name:    filepath.Base(p),
			Content: base64.StdEncoding.EncodeToString(data),
		})
	}
	switch {
	case taskSubmitPrompt != "":
		ctx := map[string]string{"prompt": taskSubmitPrompt}
		b, err := json.Marshal(ctx)
		if err != nil {
			return fmt.Errorf("encode context: %w", err)
		}
		req.Context = b
	case taskSubmitContextJSON != "":
		if !json.Valid([]byte(taskSubmitContextJSON)) {
			return fmt.Errorf("--context-json is not valid JSON")
		}
		req.Context = json.RawMessage(taskSubmitContextJSON)
	}

	client := ClientFromEnv()
	path := fmt.Sprintf("/api/v1/projects/%s/tasks", taskSubmitProject)
	resp, err := client.Post(path, req)
	if err != nil {
		return fmt.Errorf("submit task: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		// Re-wrap as a fresh response so ParseAPIError sees the body.
		r := &http.Response{StatusCode: resp.StatusCode, Body: io.NopCloser(strings.NewReader(string(body)))}
		return ParseAPIError(r)
	}
	if taskSubmitJSON {
		return prettyPrintJSON(body)
	}
	var parsed struct {
		TaskID string `json:"taskId"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	fmt.Printf("task submitted: %s (status %s)\n", parsed.TaskID, parsed.Status)
	fmt.Printf("  tail with: vornikctl task tail %s -p %s\n", parsed.TaskID, taskSubmitProject)
	return nil
}

// runTaskGet wraps GET /api/v1/projects/{p}/tasks/{id}.
func runTaskGet(cmd *cobra.Command, args []string) error {
	taskID := args[0]
	raw, err := fetchJSON(fmt.Sprintf("/api/v1/projects/%s/tasks/%s", taskGetProject, taskID))
	if err != nil {
		return err
	}
	if taskGetJSON {
		return prettyPrintJSON(raw)
	}
	var t taskResponse
	if err := json.Unmarshal(raw, &t); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	fmt.Printf("Task ID:    %s\n", t.TaskID)
	fmt.Printf("Project:    %s\n", t.ProjectID)
	fmt.Printf("Status:     %s\n", t.Status)
	fmt.Printf("Type:       %s\n", t.TaskType)
	fmt.Printf("Priority:   %d\n", t.Priority)
	fmt.Printf("Created:    %s\n", t.CreatedAt)
	if t.LastError != "" {
		fmt.Printf("Last error: %s\n", t.LastError)
	}
	return nil
}

// taskListDefaultLimit is the rows `task list` shows without --limit/--all:
// the API's own default page, so the default view is what it always was —
// but it now says when it is cut (GitHub #408).
const taskListDefaultLimit = 20

// taskListAPIPage is the page size the CLI asks for while paging: the API's
// maxPageSize. A var only so a test can force a multi-page walk; like the
// task-list flag globals, it makes those tests unsafe under t.Parallel.
var taskListAPIPage = 200

// taskListActiveStatuses is --active: persistence.CancellableTaskStatuses,
// every status an operator must CONSIDER before a cancel pass. It is not a
// promise that each row is cancellable from this CLI — `task cancel` refuses
// AWAITING_APPROVAL (answer it with approve/reject) — nor that cancelling a
// parent reaches every descendant: the cascade skips PAUSED and AWAITING_*
// children (cli-design.md "Amended 2026-10-16, GitHub #408").
func taskListActiveStatuses() string {
	sts := persistence.CancellableTaskStatuses()
	names := make([]string, len(sts))
	for i, st := range sts {
		names[i] = string(st)
	}
	return strings.Join(names, ",")
}

// runTaskList lists a project's tasks and states the cut (GitHub #408,
// cli-design.md "Amended 2026-10-16"): it pages the API until it has
// --limit rows (or every row with --all), and when fewer rows are shown than
// match, the footer says "Showing S of N tasks" instead of "Total: N".
func runTaskList(cmd *cobra.Command, _ []string) error {
	if err := validateTaskListFlags(cmd); err != nil {
		return err
	}
	status := taskListStatus
	if taskListActive {
		status = taskListActiveStatuses()
	}
	out, err := collectTaskList(ClientFromEnv(), status)
	if err != nil {
		return err
	}
	if taskListJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	printTaskList(out)
	return nil
}

func validateTaskListFlags(cmd *cobra.Command) error {
	switch {
	case taskListAll && cmd.Flags().Changed("limit"):
		return fmt.Errorf("--all and --limit are mutually exclusive")
	case taskListActive && taskListStatus != "":
		return fmt.Errorf("--active and --status are mutually exclusive")
	case !taskListAll && taskListLimit < 1:
		return fmt.Errorf("--limit must be at least 1")
	case taskListOffset < 0:
		return fmt.Errorf("--offset must not be negative")
	}
	return nil
}

// collectTaskList walks the API's pages from --offset, de-duplicating by
// task ID, and decides whether the result is cut.
func collectTaskList(client *Client, status string) (taskListOutput, error) {
	base := fmt.Sprintf("/api/v1/projects/%s/tasks", url.PathEscape(taskListProject))
	var (
		rows      []taskResponse
		seen      = map[string]bool{}
		total     int
		offset    = taskListOffset
		unknown   bool // a page's count failed: total is not the whole set
		haveTotal bool // some page carried a real count
		atEnd     bool // the walk reached the end of the matching rows
	)
	for {
		want := taskListAPIPage
		if !taskListAll {
			want = min(taskListLimit-len(rows), taskListAPIPage)
		}
		q := url.Values{}
		if status != "" {
			q.Set("status", status)
		}
		q.Set("pageSize", strconv.Itoa(want))
		q.Set("offset", strconv.Itoa(offset))
		page, err := fetchTaskPage(client, base+"?"+q.Encode())
		if err != nil {
			return taskListOutput{}, err
		}
		// A real count is kept for the rest of the walk; a later page's
		// fallback (the page length, when its count failed) never replaces
		// it. With no real count yet, total is the position reached — a lower
		// bound, flagged by unknown.
		if page.TotalUnknown {
			unknown = true
			if !haveTotal {
				total = max(total, offset+len(page.Tasks))
			}
		} else {
			total, haveTotal = page.Total, true
		}
		for _, t := range page.Tasks {
			if !seen[t.TaskID] {
				seen[t.TaskID] = true
				rows = append(rows, t)
			}
		}
		offset += len(page.Tasks)
		if taskPageIsLast(page, offset, want) {
			atEnd = true
			break
		}
		if !taskListAll && len(rows) >= taskListLimit {
			break
		}
	}

	out := taskListOutput{Tasks: rows, Total: total, Shown: len(rows), TotalUnknown: unknown}
	if out.Tasks == nil {
		out.Tasks = []taskResponse{}
	}
	// The cut is measured against what matches beyond --offset.
	matching := max(total-taskListOffset, 0)
	if (unknown && !atEnd) || (!unknown && len(rows) < matching) {
		out.Truncated = true
		next := offset
		out.NextOffset = &next
	}
	return out, nil
}

// taskPageIsLast: with a known total the walk ends on reaching it; with an
// unknown one only an empty or short page proves the end.
func taskPageIsLast(page listTasksResponse, offset, want int) bool {
	if len(page.Tasks) == 0 {
		return true
	}
	if page.TotalUnknown {
		return len(page.Tasks) < want
	}
	return offset >= page.Total
}

// printTaskList renders the table and the footer that states the cut.
func printTaskList(out taskListOutput) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TASK ID\tSTATUS\tTYPE\tPRIORITY\tPARENT\tCREATED AT")
	for _, task := range out.Tasks {
		parent := task.ParentTaskID
		if parent == "" {
			parent = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			task.TaskID, task.Status, task.TaskType, task.Priority, parent, task.CreatedAt)
	}
	_ = w.Flush()

	switch {
	case out.TotalUnknown:
		more := "the list reached its end"
		if out.Truncated {
			more = "more may match; pass --all (or --limit N) to see the rest"
		}
		fmt.Printf("\nShowing %d tasks; the daemon could not count the total, so %s.\n", out.Shown, more)
	case out.Truncated:
		fmt.Printf("\nShowing %d of %d tasks; pass --all (or --limit N) to see the rest.\n",
			out.Shown, max(out.Total-taskListOffset, 0))
	case taskListOffset > 0:
		// Every match from --offset on is shown; say how many that is, so
		// "Total: 32" never sits under 22 rows.
		fmt.Printf("\nTotal: %d (%d shown from offset %d)\n", out.Total, out.Shown, taskListOffset)
	default:
		fmt.Printf("\nTotal: %d\n", out.Total)
	}
}

// fetchTaskPage GETs one page of the task list.
func fetchTaskPage(client *Client, path string) (listTasksResponse, error) {
	var page listTasksResponse
	resp, err := client.Get(path)
	if err != nil {
		return page, fmt.Errorf("failed to list tasks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return page, ParseAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return page, fmt.Errorf("failed to read response: %w", err)
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return page, fmt.Errorf("failed to parse response: %w", err)
	}
	return page, nil
}

func runTaskCancel(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	taskID := args[0]

	path := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/cancel", taskCancelProject, taskID)

	resp, err := client.Post(path, nil)
	if err != nil {
		return fmt.Errorf("failed to cancel task: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	var result cancelTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	fmt.Printf("Task %s cancelled successfully\n", result.TaskID)
	fmt.Printf("Status: %s\n", result.Status)
	fmt.Printf("Was running: %v\n", result.WasRunning)
	fmt.Printf("Cancelled at: %s\n", result.CancelledAt)

	return nil
}

func runTaskRetry(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	taskID := args[0]

	path := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/retry", taskRetryProject, taskID)

	var body interface{}
	if taskRetryResetFlag {
		body = map[string]bool{"resetAttempts": true}
	}

	resp, err := client.Post(path, body)
	if err != nil {
		return fmt.Errorf("failed to retry task: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return ParseAPIError(resp)
	}

	var result retryTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	fmt.Printf("Task %s retried successfully\n", result.TaskID)
	fmt.Printf("Status: %s\n", result.Status)
	fmt.Printf("Attempt: %d\n", result.Attempt)
	fmt.Printf("Retried at: %s\n", result.RetriedAt)

	return nil
}

func runTaskTail(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	taskID := args[0]
	path := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/logs?tail=%d", taskTailProject, taskID, taskTailLines)

	printOnce := func(last string) (string, error) {
		resp, err := client.Get(path)
		if err != nil {
			return last, fmt.Errorf("failed to fetch logs: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return last, ParseAPIError(resp)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return last, fmt.Errorf("failed to read logs: %w", err)
		}
		text := string(data)
		if !taskTailFollow {
			fmt.Print(text)
			if text != "" && !strings.HasSuffix(text, "\n") {
				fmt.Println()
			}
			return text, nil
		}
		if strings.HasPrefix(text, last) {
			fmt.Print(strings.TrimPrefix(text, last))
		} else if text != last {
			if last != "" {
				fmt.Println("\n--- log window reset ---")
			}
			fmt.Print(text)
		}
		return text, nil
	}

	last, err := printOnce("")
	if err != nil || !taskTailFollow {
		return err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-cmd.Context().Done():
			return nil
		case <-ticker.C:
			var err error
			last, err = printOnce(last)
			if err != nil {
				return err
			}
		}
	}
}
