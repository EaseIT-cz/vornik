package forge

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"vornik.io/vornik/internal/aidisclosure"
)

// Hard caps bound every issue API read.
const (
	MaxIssuePages     = 10
	MaxIssuePageSize  = 100
	MaxIssueComments  = 100
	MaxIssueTextBytes = 16 << 10
)

// IssueReader is optional: existing ForgeProvider consumers remain unchanged.
type IssueReader interface {
	ListIssues(context.Context, string, IssueFilter) (IssueList, error)
	GetIssue(context.Context, string, int) (Issue, error)
	ListLabels(context.Context, string) (LabelList, error)
	VerifyIssueAccess(context.Context, bool) error
}

// IssueWriter optionally exposes daemon-side mutations.
type IssueWriter interface {
	AddLabels(context.Context, string, int, []string) error
	SetLabels(context.Context, string, int, []string) error
	CloseIssue(context.Context, string, int, CloseIssueSpec) error
}

// IssueFilter selects a bounded upstream issue page.
type IssueFilter struct {
	State   string   `json:"state,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	Since   string   `json:"since,omitempty"`
	Page    int      `json:"page,omitempty"`
	PerPage int      `json:"per_page,omitempty"`
}

// Issue contains third-party issue data; consumers must mark it untrusted.
type Issue struct {
	Number            int            `json:"number"`
	Title             string         `json:"title"`
	Body              string         `json:"body"`
	State             string         `json:"state"`
	Author            string         `json:"author"`
	Labels            []string       `json:"labels"`
	CreatedAt         string         `json:"created_at"`
	UpdatedAt         string         `json:"updated_at"`
	ClosedAt          string         `json:"closed_at,omitempty"`
	Comments          []IssueComment `json:"comments,omitempty"`
	CommentCount      int            `json:"comment_count"`
	CommentsTruncated bool           `json:"comments_truncated,omitempty"`
	Truncated         bool           `json:"truncated,omitempty"`
}

// IssueComment includes verified App attribution used for retry markers.
type IssueComment struct {
	AppID     int64  `json:"app_id,omitempty"`
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Truncated bool   `json:"truncated,omitempty"`
}

// IssueList preserves upstream pagination after excluding pull requests.
type IssueList struct {
	Issues    []Issue `json:"issues"`
	Scanned   int     `json:"scanned"`
	NextPage  int     `json:"next_page,omitempty"`
	Truncated bool    `json:"truncated,omitempty"`
}

// Label describes a repository label as third-party data.
type Label struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Color       string `json:"color"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// LabelList reports a capped repository label scan.
type LabelList struct {
	Labels    []Label `json:"labels"`
	Scanned   int     `json:"scanned"`
	Truncated bool    `json:"truncated,omitempty"`
}

// CloseIssueSpec names the duplicate target and explanation.
type CloseIssueSpec struct {
	DuplicateOf int    `json:"duplicate_of"`
	Explanation string `json:"explanation"`
	// Notice is the Art 50 publication notice the posted comment carries,
	// filled by the daemon from its disclosure service — never from agent
	// input, hence not JSON-bound. Empty means the compiled default (Art 50
	// design 2026-07-28 §11.4, GitHub #342).
	Notice aidisclosure.Notice `json:"-"`
}

var repoSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidateIssueRepo rejects path and query injection before networking.
func ValidateIssueRepo(repo string) error {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || len(repo) > 256 {
		return fmt.Errorf("forge: invalid owner/repo")
	}
	for _, p := range parts {
		if !repoSegment.MatchString(p) {
			return fmt.Errorf("forge: invalid owner/repo")
		}
	}
	return nil
}

// ValidateIssueLabels validates bounded exact label names.
func ValidateIssueLabels(labels []string) error {
	if len(labels) > 100 {
		return fmt.Errorf("forge: too many labels")
	}
	for _, l := range labels {
		if strings.TrimSpace(l) == "" || len(l) > 100 || !utf8.ValidString(l) || strings.ContainsAny(l, "\r\n\x00") {
			return fmt.Errorf("forge: invalid label")
		}
	}
	return nil
}

// Resolve defaults and validates a bounded issue filter.
func (f IssueFilter) Resolve() (IssueFilter, error) {
	if f.Page == 0 {
		f.Page = 1
	}
	if f.PerPage == 0 {
		f.PerPage = 30
	}
	if f.State == "" {
		f.State = "open"
	}
	if f.Page < 1 || f.Page > MaxIssuePages || f.PerPage < 1 || f.PerPage > MaxIssuePageSize {
		return f, fmt.Errorf("forge: pagination exceeds caps")
	}
	if f.State != "open" && f.State != "closed" && f.State != "all" {
		return f, fmt.Errorf("forge: invalid issue state")
	}
	if f.Since != "" {
		if _, err := time.Parse(time.RFC3339, f.Since); err != nil {
			return f, fmt.Errorf("forge: since must be RFC3339")
		}
	}
	return f, ValidateIssueLabels(f.Labels)
}

// ClipIssueText clips on a UTF-8 boundary; caller carries the truncation flag.
func ClipIssueText(s string) (string, bool) {
	if len(s) <= MaxIssueTextBytes {
		return s, false
	}
	n := MaxIssueTextBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
