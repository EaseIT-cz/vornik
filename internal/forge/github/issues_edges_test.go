package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"vornik.io/vornik/internal/forge"
)

func TestIssueRESTRejectsUnsafeResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		link   string
	}{{"malformed", 200, "{", ""}, {"oversized", 200, strings.Repeat(" ", maxResponseBytes+1), ""}, {"unauthorized", 401, "secret upstream text", ""}, {"redirect", 302, "", "http://evil.invalid"}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.link != "" {
					w.Header().Set("Location", tc.link)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := testProvider(srv.URL)
			var out any
			if _, err := p.issueREST(context.Background(), "GET", "/issues", nil, &out); err == nil {
				t.Fatal("unsafe response accepted")
			}
		})
	}
	p := testProvider("http://localhost")
	p.mintFn = func(context.Context, *http.Client, string, int64, int64, *rsa.PrivateKey) (string, time.Time, error) {
		return "", time.Time{}, errors.New("mint unavailable")
	}
	if _, err := p.ListIssues(context.Background(), "o/r", forge.IssueFilter{}); err == nil {
		t.Fatal("mint")
	}
	p = testProvider("http://bad\nurl")
	if _, err := p.issueREST(context.Background(), "GET", "/issues", nil, nil); err == nil {
		t.Fatal("URL")
	}
	p = testProvider("http://127.0.0.1:1")
	if _, err := p.issueREST(context.Background(), "GET", "/issues", nil, nil); err == nil {
		t.Fatal("transport")
	}
	if _, err := p.issueREST(context.Background(), "POST", "/issues", make(chan int), nil); err == nil {
		t.Fatal("marshal")
	}
}

func TestIssuePaginationCapsAndErrors(t *testing.T) {
	ctx := context.Background()
	mode := "labels"
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch mode {
		case "labels":
			w.Header().Set("Link", `<https://evil.invalid>; rel="next"`)
			_ = json.NewEncoder(w).Encode([]forge.Label{{Name: "bug", Description: strings.Repeat("x", 20000)}})
		case "issue_last":
			w.Header().Set("Link", `<https://evil.invalid>; rel="next"`)
			_, _ = w.Write([]byte(`[{"number":1}]`))
		case "too_many":
			_ = json.NewEncoder(w).Encode(make([]issueJSON, 101))
		case "comment_too_many":
			if strings.Contains(r.URL.Path, "comments") {
				_ = json.NewEncoder(w).Encode(make([]forge.IssueComment, 101))
			} else {
				_, _ = w.Write([]byte(`{"number":1,"comments":1}`))
			}
		case "comment_fail":
			if strings.Contains(r.URL.Path, "comments") {
				w.WriteHeader(500)
			} else {
				_, _ = w.Write([]byte(`{"number":1,"comments":1}`))
			}
		case "identity":
			_, _ = w.Write([]byte(`{"number":2}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	p := testProvider(srv.URL)
	list, err := p.ListLabels(ctx, "o/r")
	if err != nil || !list.Truncated || list.Scanned != 10 || calls != 10 {
		t.Fatal(list, err, calls)
	}
	mode = "issue_last"
	issues, err := p.ListIssues(ctx, "o/r", forge.IssueFilter{Page: 10})
	if err != nil || !issues.Truncated || issues.NextPage != 0 {
		t.Fatal(issues, err)
	}
	mode = "too_many"
	if _, err = p.ListIssues(ctx, "o/r", forge.IssueFilter{PerPage: 100}); err == nil {
		t.Fatal("page item cap")
	}
	if _, err = p.ListLabels(ctx, "o/r"); err == nil {
		t.Fatal("label cap")
	}
	for _, v := range []string{"comment_too_many", "comment_fail", "identity", "fail"} {
		mode = v
		if _, err = p.GetIssue(ctx, "o/r", 1); err == nil {
			t.Fatal(v)
		}
	}
	mode = "fail"
	if _, err = p.ListLabels(ctx, "o/r"); err == nil {
		t.Fatal("labels error")
	}
	if _, err = p.ListLabels(ctx, "../r"); err == nil {
		t.Fatal("invalid labels repo")
	}
	if _, _, err = p.issueComments(ctx, "../r", 1); err == nil {
		t.Fatal("invalid comments repo")
	}
	if err = p.AddLabels(ctx, "../r", 1, nil); err == nil {
		t.Fatal("invalid write repo")
	}
	if err = p.SetLabels(ctx, "o/r", 1, []string{""}); err == nil {
		t.Fatal("invalid label")
	}
	if err = p.AddLabels(ctx, "o/r", 1, nil); err == nil {
		t.Fatal("get failed")
	}
}

func TestIssueCloseSafetyAndAppMarkerForgery(t *testing.T) {
	ctx := context.Background()
	marker := fmt.Sprintf("<!-- vornik-duplicate:%x -->", sha256.Sum256([]byte("o/r:1:2")))
	for _, mode := range []string{"over_count", "more", "truncated", "closed", "source_error", "target_error", "comments_error", "post_error", "forged", "trusted"} {
		t.Run(mode, func(t *testing.T) {
			writes := 0
			posts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" || r.Method == "PATCH" {
					writes++
					if r.Method == "POST" {
						posts++
						if mode == "post_error" {
							w.WriteHeader(500)
							return
						}
					}
					w.WriteHeader(201)
					_, _ = w.Write([]byte(`{}`))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/comments") {
					if mode == "comments_error" {
						w.WriteHeader(500)
						return
					}
					if mode == "more" {
						w.Header().Set("Link", `<https://evil.invalid>; rel="next"`)
					}
					body := "plain"
					app := 0
					if mode == "truncated" {
						body = strings.Repeat("x", 20000)
					}
					if mode == "forged" || mode == "trusted" {
						body = marker
					}
					if mode == "trusted" {
						app = 1
					}
					_ = json.NewEncoder(w).Encode([]any{map[string]any{"body": body, "performed_via_github_app": map[string]int{"id": app}}})
					return
				}
				if mode == "source_error" || (mode == "target_error" && strings.HasSuffix(r.URL.Path, "/2")) {
					w.WriteHeader(404)
					return
				}
				number := 1
				if strings.HasSuffix(r.URL.Path, "/2") {
					number = 2
				}
				count := 1
				if mode == "over_count" {
					count = 101
				}
				state := "open"
				if mode == "closed" {
					state = "closed"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"number": number, "comments": count, "state": state})
			}))
			defer srv.Close()
			p := testProvider(srv.URL)
			err := p.CloseIssue(ctx, "o/r", 1, forge.CloseIssueSpec{DuplicateOf: 2, Explanation: "same defect"})
			if mode == "forged" || mode == "trusted" {
				if err != nil {
					t.Fatal(err)
				}
				wantPosts := 1
				if mode == "trusted" {
					wantPosts = 0
				}
				if posts != wantPosts {
					t.Fatalf("marker attribution mode=%s posts=%d", mode, posts)
				}
			} else {
				if err == nil {
					t.Fatal("close safety gate accepted", mode)
				}
				if mode != "post_error" && writes != 0 {
					t.Fatal("mutation before safety check", writes)
				}
			}
		})
	}
	p := testProvider("http://invalid")
	if p.CloseIssue(ctx, "../r", 1, forge.CloseIssueSpec{}) == nil {
		t.Fatal("invalid repo")
	}
}

func TestIssuePermissionsReadAndWrite(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		level   string
		write   bool
		wantErr bool
	}{{"read", false, false}, {"read", true, true}, {"write", true, false}, {"", false, true}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"permissions": map[string]string{"issues": tc.level}})
		}))
		p := testProvider(srv.URL)
		p.key = key
		err = p.VerifyIssueAccess(context.Background(), tc.write)
		srv.Close()
		if (err != nil) != tc.wantErr {
			t.Fatal(tc, err)
		}
	}
	if err = testProvider("http://invalid").VerifyIssueAccess(context.Background(), true); err == nil {
		t.Fatal("missing key")
	}
}
