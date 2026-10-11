package github

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
	"vornik.io/vornik/internal/forge"
	ghapp "vornik.io/vornik/internal/github"
)

// A provider serializes issue changes; no agent receives its credentials.
var _ forge.IssueReader = (*Provider)(nil)
var _ forge.IssueWriter = (*Provider)(nil)

type issueJSON struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels      []ghLabel       `json:"labels"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	ClosedAt    string          `json:"closed_at"`
	Comments    int             `json:"comments"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func (v issueJSON) neutral() forge.Issue {
	out := forge.Issue{Number: v.Number, State: v.State, Author: v.User.Login, Labels: labelNames(v.Labels), CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, ClosedAt: v.ClosedAt, CommentCount: v.Comments}
	var clipped bool
	out.Title, out.Truncated = forge.ClipIssueText(v.Title)
	out.Body, clipped = forge.ClipIssueText(v.Body)
	out.Truncated = out.Truncated || clipped
	return out
}
func (p *Provider) issueREST(ctx context.Context, method, path string, payload any, out any) (http.Header, error) {
	tok, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	var body []byte
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	req, err := p.newReq(ctx, method, p.apiBaseURL+path, body, tok)
	if err != nil {
		return nil, err
	}
	// Do not allow a credential-bearing redirect to leave the configured API.
	client := *p.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forge/github: issue request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("forge/github: issue response exceeds size cap")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("forge/github: issue operation HTTP %d", resp.StatusCode)
	}
	if out != nil {
		if err = json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("forge/github: invalid issue response: %w", err)
		}
	}
	return resp.Header, nil
}
func issuePath(repo string, number int) (string, error) {
	if err := forge.ValidateIssueRepo(repo); err != nil {
		return "", err
	}
	if number < 1 {
		return "", fmt.Errorf("forge: invalid issue number")
	}
	return "/repos/" + repo + "/issues/" + strconv.Itoa(number), nil
}

// ListIssues fetches one capped issue page, excluding pull requests.
func (p *Provider) ListIssues(ctx context.Context, repo string, f forge.IssueFilter) (forge.IssueList, error) {
	out := forge.IssueList{Issues: []forge.Issue{}}
	if err := forge.ValidateIssueRepo(repo); err != nil {
		return out, err
	}
	f, err := f.Resolve()
	if err != nil {
		return out, err
	}
	q := url.Values{"state": {f.State}, "page": {strconv.Itoa(f.Page)}, "per_page": {strconv.Itoa(f.PerPage)}}
	if len(f.Labels) > 0 {
		q.Set("labels", strings.Join(f.Labels, ","))
	}
	if f.Since != "" {
		q.Set("since", f.Since)
	}
	var items []issueJSON
	h, err := p.issueREST(ctx, http.MethodGet, "/repos/"+repo+"/issues?"+q.Encode(), nil, &items)
	if err != nil {
		return out, err
	}
	if len(items) > f.PerPage {
		return out, fmt.Errorf("forge/github: issue page exceeds item cap")
	}
	out.Scanned = len(items)
	for _, v := range items {
		if len(v.PullRequest) > 0 && string(v.PullRequest) != "null" {
			continue
		}
		i := v.neutral()
		out.Truncated = out.Truncated || i.Truncated
		out.Issues = append(out.Issues, i)
	}
	if strings.Contains(h.Get("Link"), `rel="next"`) {
		if f.Page < forge.MaxIssuePages {
			out.NextPage = f.Page + 1
		} else {
			out.Truncated = true
		}
	}
	return out, nil
}
func (p *Provider) getIssue(ctx context.Context, repo string, number int) (forge.Issue, error) {
	path, err := issuePath(repo, number)
	if err != nil {
		return forge.Issue{}, err
	}
	var v issueJSON
	if _, err = p.issueREST(ctx, http.MethodGet, path, nil, &v); err != nil {
		return forge.Issue{}, err
	}
	if len(v.PullRequest) > 0 && string(v.PullRequest) != "null" {
		return forge.Issue{}, fmt.Errorf("forge: pull requests are not issues")
	}
	if v.Number != number {
		return forge.Issue{}, fmt.Errorf("forge: issue identity mismatch")
	}
	return v.neutral(), nil
}
func (p *Provider) issueComments(ctx context.Context, repo string, number int) ([]forge.IssueComment, bool, error) {
	path, err := issuePath(repo, number)
	if err != nil {
		return nil, false, err
	}
	var items []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		App  struct {
			ID int64 `json:"id"`
		} `json:"performed_via_github_app"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	h, err := p.issueREST(ctx, http.MethodGet, path+"/comments?per_page=100&page=1", nil, &items)
	if err != nil {
		return nil, false, err
	}
	if len(items) > forge.MaxIssueComments {
		return nil, false, fmt.Errorf("forge: comments exceeds cap")
	}
	out := make([]forge.IssueComment, 0, len(items))
	for _, v := range items {
		body, trunc := forge.ClipIssueText(v.Body)
		out = append(out, forge.IssueComment{AppID: v.App.ID, ID: v.ID, Body: body, Author: v.User.Login, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, Truncated: trunc})
	}
	return out, strings.Contains(h.Get("Link"), `rel="next"`), nil
}

// GetIssue returns bounded issue text and comments, rejecting pull requests.
func (p *Provider) GetIssue(ctx context.Context, repo string, number int) (forge.Issue, error) {
	out, err := p.getIssue(ctx, repo, number)
	if err != nil {
		return out, err
	}
	if out.CommentCount > 0 {
		comments, more, err := p.issueComments(ctx, repo, number)
		if err != nil {
			return out, err
		}
		out.Comments = comments
		out.CommentsTruncated = more || out.CommentCount > forge.MaxIssueComments
		for _, c := range comments {
			out.Truncated = out.Truncated || c.Truncated
		}
	}
	return out, nil
}

// ListLabels scans at most ten label pages.
func (p *Provider) ListLabels(ctx context.Context, repo string) (forge.LabelList, error) {
	out := forge.LabelList{Labels: []forge.Label{}}
	if err := forge.ValidateIssueRepo(repo); err != nil {
		return out, err
	}
	for page := 1; page <= forge.MaxIssuePages; page++ {
		var items []forge.Label
		h, err := p.issueREST(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/labels?per_page=100&page=%d", repo, page), nil, &items)
		if err != nil {
			return out, err
		}
		if len(items) > 100 {
			return out, fmt.Errorf("forge: labels page exceeds cap")
		}
		out.Scanned += len(items)
		for _, v := range items {
			var trunc bool
			v.Description, trunc = forge.ClipIssueText(v.Description)
			v.Truncated = trunc
			v.Name, trunc = forge.ClipIssueText(v.Name)
			v.Truncated = v.Truncated || trunc
			out.Truncated = out.Truncated || v.Truncated
			out.Labels = append(out.Labels, v)
		}
		if !strings.Contains(h.Get("Link"), `rel="next"`) {
			return out, nil
		}
		if page == forge.MaxIssuePages {
			out.Truncated = true
		}
	}
	return out, nil
}

// AddLabels adds exact labels idempotently to an issue.
func (p *Provider) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	return p.writeLabels(ctx, http.MethodPost, repo, number, labels)
}

// SetLabels applies an absolute label set to an issue.
func (p *Provider) SetLabels(ctx context.Context, repo string, number int, labels []string) error {
	return p.writeLabels(ctx, http.MethodPut, repo, number, labels)
}
func (p *Provider) writeLabels(ctx context.Context, method, repo string, number int, labels []string) error {
	path, err := issuePath(repo, number)
	if err != nil {
		return err
	}
	if err = forge.ValidateIssueLabels(labels); err != nil {
		return err
	}
	if _, err = p.getIssue(ctx, repo, number); err != nil {
		return err
	}
	if labels == nil {
		labels = []string{}
	}
	_, err = p.issueREST(ctx, method, path+"/labels", map[string]any{"labels": labels}, nil)
	return err
}

// CloseIssue serializes close check/comment/close across operations in this instance.
// GitHub comment POST has no cross-replica idempotency key. Closing requires
// one active API instance; concurrent replicas may post duplicate explanations.
func (p *Provider) CloseIssue(ctx context.Context, repo string, number int, s forge.CloseIssueSpec) error {
	path, err := issuePath(repo, number)
	if err != nil {
		return err
	}
	if s.DuplicateOf < 1 || s.DuplicateOf == number || strings.TrimSpace(s.Explanation) == "" || len(s.Explanation) > forge.MaxIssueTextBytes || !utf8.ValidString(s.Explanation) {
		return fmt.Errorf("forge: invalid duplicate explanation/reference")
	}
	p.issueMu.Lock()
	defer p.issueMu.Unlock()
	issue, err := p.getIssue(ctx, repo, number)
	if err != nil {
		return err
	}
	if _, err = p.getIssue(ctx, repo, s.DuplicateOf); err != nil {
		return err
	}
	marker := fmt.Sprintf("<!-- vornik-duplicate:%x -->", sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", strings.ToLower(repo), number, s.DuplicateOf))))
	if issue.CommentCount > forge.MaxIssueComments {
		return fmt.Errorf("forge: cannot verify duplicate marker beyond comment cap")
	}
	comments, more, err := p.issueComments(ctx, repo, number)
	if err != nil {
		return err
	}
	if more {
		return fmt.Errorf("forge: cannot verify full comment set")
	}
	found := false
	for _, c := range comments {
		if c.Truncated {
			return fmt.Errorf("forge: cannot verify truncated comment marker")
		}
		if c.AppID == p.appID && strings.Contains(c.Body, marker) {
			found = true
		}
	}
	if issue.State == "closed" && !found {
		return fmt.Errorf("forge: issue already closed without this duplicate reference")
	}
	if !found {
		body := marker + "\nDuplicate of #" + strconv.Itoa(s.DuplicateOf) + ".\n\n" + s.Explanation + "\n\nAutomated triage by Vornik."
		if _, err = p.issueREST(ctx, http.MethodPost, path+"/comments", map[string]string{"body": body}, nil); err != nil {
			return err
		}
	}
	if issue.State == "closed" {
		return nil
	}
	_, err = p.issueREST(ctx, http.MethodPatch, path, map[string]string{"state": "closed", "state_reason": "not_planned"}, nil)
	return err
}

// VerifyIssueAccess reports missing App issue permissions without mutation.
func (p *Provider) VerifyIssueAccess(ctx context.Context, write bool) error {
	level, err := ghapp.CheckInstallationPermission(ctx, p.httpClient, p.apiBaseURL, p.appID, p.installationID, p.key, "issues")
	if err != nil {
		return err
	}
	if level != "write" && (write || level != "read") {
		return fmt.Errorf("forge/github: Issues permission missing; grant Issues read%s and reaccept installation", map[bool]string{true: " & write", false: ""}[write])
	}
	return nil
}
