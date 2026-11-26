package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// GitHub #792 (design 27 §C4): bind a companion key to one GitHub Actions
// workflow, so the job authenticates with its run's ID token instead of a
// stored secret.

var (
	trustKey, trustProject, trustRepo, trustRef, trustWorkflow, trustEvents, trustAudience string
	trustRepoID                                                                            int64
	trustsProject                                                                          string
	trustsJSON                                                                             bool
	untrustProject                                                                         string
)

var companionTrustGitHubCmd = &cobra.Command{
	Use:   "trust-github",
	Short: "Bind a companion key to one GitHub Actions workflow (the job needs no stored secret)",
	Long: `Bind a companion key to one workflow file on one branch of one repository.
A job of that workflow then authenticates with the ID token GitHub issues to
its run (permissions: id-token: write), requested for --audience. Every claim
must match exactly; the key's project, workflows, budget and limit apply.

--repo-id is the repository's numeric id, which survives a rename:
  gh api repos/<owner>/<name> --jq .id`,
	RunE: runCompanionTrustGitHub,
}

var companionTrustsCmd = &cobra.Command{
	Use:   "trusts",
	Short: "List a project's GitHub Actions trusts, with when each last served a request",
	RunE:  runCompanionTrusts,
}

var companionUntrustCmd = &cobra.Command{
	Use:   "untrust <trust id>",
	Short: "Remove a GitHub Actions trust",
	Args:  cobra.ExactArgs(1),
	RunE:  runCompanionUntrust,
}

func init() {
	f := companionTrustGitHubCmd.Flags()
	f.StringVar(&trustKey, "key", "", "Companion key id (required)")
	f.StringVarP(&trustProject, "project", "p", "", "The key's project (required)")
	f.StringVar(&trustRepo, "repo", "", "Repository, owner/name (required)")
	f.Int64Var(&trustRepoID, "repo-id", 0, "The repository's numeric id (required)")
	f.StringVar(&trustRef, "ref", "refs/heads/main", "Branch or tag ref the workflow runs on")
	f.StringVar(&trustWorkflow, "workflow", "", "Workflow file, .github/workflows/<file>.yml (required)")
	f.StringVar(&trustEvents, "event", "", "Comma-separated events (default push,schedule,workflow_dispatch; pull_request is refused)")
	f.StringVar(&trustAudience, "audience", "", "Token audience the job requests (default: this Vornik's URL)")
	for _, n := range []string{"key", "project", "repo", "repo-id", "workflow"} {
		_ = companionTrustGitHubCmd.MarkFlagRequired(n)
	}
	companionTrustsCmd.Flags().StringVarP(&trustsProject, "project", "p", "", "Project ID (required)")
	companionTrustsCmd.Flags().BoolVar(&trustsJSON, "json", false, "Emit JSON")
	_ = companionTrustsCmd.MarkFlagRequired("project")
	companionUntrustCmd.Flags().StringVarP(&untrustProject, "project", "p", "", "Project ID (required)")
	_ = companionUntrustCmd.MarkFlagRequired("project")
	companionCmd.AddCommand(companionTrustGitHubCmd, companionTrustsCmd, companionUntrustCmd)
}

type trustOut struct {
	ID             string     `json:"id"`
	KeyID          string     `json:"keyId"`
	Repository     string     `json:"repository"`
	RepositoryID   int64      `json:"repositoryId"`
	Ref            string     `json:"ref"`
	JobWorkflowRef string     `json:"jobWorkflowRef"`
	Events         []string   `json:"events"`
	Audience       string     `json:"audience"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastUsedAt     *time.Time `json:"lastUsedAt"`
}

func runCompanionTrustGitHub(_ *cobra.Command, _ []string) error {
	client := ClientFromEnv()
	aud := trustAudience
	if aud == "" {
		aud = strings.TrimSuffix(client.baseURL, "/")
	}
	var events []string
	for _, e := range strings.Split(trustEvents, ",") {
		if e = strings.TrimSpace(e); e != "" {
			events = append(events, e)
		}
	}
	resp, err := client.Post("/api/v1/companion/trusts", map[string]any{
		"projectId": trustProject, "keyId": trustKey, "repository": trustRepo, "repositoryId": trustRepoID,
		"ref": trustRef, "workflow": trustWorkflow, "events": events, "audience": aud,
	})
	if err != nil {
		return fmt.Errorf("trust: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return ParseAPIError(resp)
	}
	var t trustOut
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	fmt.Printf("Trust %s: key %s serves %s on %s (%s), events %s, audience %s\n",
		t.ID, t.KeyID, t.JobWorkflowRef, t.Repository, fmt.Sprint(t.RepositoryID), strings.Join(t.Events, ","), t.Audience)
	fmt.Println("In the workflow: permissions: id-token: write, then request a token for that audience and send it as Authorization: Bearer <token>.")
	return nil
}

func runCompanionTrusts(_ *cobra.Command, _ []string) error {
	resp, err := ClientFromEnv().Get("/api/v1/companion/trusts?projectId=" + url.QueryEscape(trustsProject))
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	var out struct {
		Trusts []trustOut `json:"trusts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if trustsJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if len(out.Trusts) == 0 {
		fmt.Printf("No GitHub Actions trusts in project %q.\n", trustsProject)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKEY\tWORKFLOW\tREPO ID\tEVENTS\tLAST USED")
	for _, t := range out.Trusts {
		last := "never"
		if t.LastUsedAt != nil {
			last = t.LastUsedAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", t.ID, t.KeyID, t.JobWorkflowRef, t.RepositoryID, strings.Join(t.Events, ","), last)
	}
	return tw.Flush()
}

func runCompanionUntrust(_ *cobra.Command, args []string) error {
	resp, err := ClientFromEnv().Delete("/api/v1/companion/trusts?projectId=" + url.QueryEscape(untrustProject) + "&id=" + url.QueryEscape(args[0]))
	if err != nil {
		return fmt.Errorf("untrust: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return ParseAPIError(resp)
	}
	fmt.Printf("Removed trust %s.\n", args[0])
	return nil
}
