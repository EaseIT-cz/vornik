package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
	"vornik.io/vornik/internal/forge"
)

func TestIssueReadsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token ghs_test" {
			t.Error("missing App token")
		}
		switch r.URL.Path {
		case "/repos/o/r/issues":
			if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("per_page") != "2" {
				t.Error(r.URL)
			}
			w.Header().Set("Link", `<https://evil.invalid>; rel="next"`)
			_, _ = w.Write([]byte(`[{"number":1,"title":"issue"},{"number":2,"pull_request":{}}]`))
		case "/repos/o/r/issues/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "body": strings.Repeat("é", 10000), "comments": 101, "labels": []any{map[string]string{"name": "bug"}}})
		case "/repos/o/r/issues/1/comments":
			if r.URL.Query().Get("per_page") != "100" {
				t.Error("comment cap")
			}
			_, _ = w.Write([]byte(`[{"body":"hello","user":{"login":"alice"}}]`))
		case "/repos/o/r/issues/2":
			_, _ = w.Write([]byte(`{"number":2,"pull_request":{}}`))
		case "/repos/o/r/labels":
			_, _ = w.Write([]byte(`[{"name":"bug","description":"untrusted"}]`))
		default:
			t.Error(r.URL)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	p := testProvider(srv.URL)
	list, err := p.ListIssues(context.Background(), "o/r", forge.IssueFilter{Page: 2, PerPage: 2})
	if err != nil || len(list.Issues) != 1 || list.Scanned != 2 || list.NextPage != 3 {
		t.Fatalf("%+v %v", list, err)
	}
	issue, err := p.GetIssue(context.Background(), "o/r", 1)
	if err != nil || !issue.Truncated || !issue.CommentsTruncated || !utf8.ValidString(issue.Body) || len(issue.Body) > forge.MaxIssueTextBytes || len(issue.Comments) != 1 {
		t.Fatalf("%+v %v", issue, err)
	}
	if _, err := p.GetIssue(context.Background(), "o/r", 2); err == nil {
		t.Fatal("PR accepted")
	}
	labels, err := p.ListLabels(context.Background(), "o/r")
	if err != nil || len(labels.Labels) != 1 {
		t.Fatal(labels, err)
	}
}

func TestIssueInvalidInputsDoNotNetwork(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(500) }))
	defer srv.Close()
	p := testProvider(srv.URL)
	for _, repo := range []string{"../r", "o/r?x=y", "o/r/extra", "o/%2Fr", "o/r#x", "o/."} {
		if _, err := p.ListIssues(context.Background(), repo, forge.IssueFilter{}); err == nil {
			t.Fatal(repo)
		}
	}
	for _, f := range []forge.IssueFilter{{Page: 11}, {PerPage: 101}, {Page: -1}, {State: "bad"}, {Since: "yesterday"}} {
		if _, err := p.ListIssues(context.Background(), "o/r", f); err == nil {
			t.Fatal(f)
		}
	}
	if _, err := p.GetIssue(context.Background(), "o/r", 0); err == nil {
		t.Fatal("zero")
	}
	if calls != 0 {
		t.Fatal(calls)
	}
}

func TestIssueWritesAndCloseRetry(t *testing.T) {
	var comments []map[string]any
	state := "open"
	var labels []string
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/issues/1/labels":
			var v struct {
				Labels []string `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&v)
			labels = v.Labels
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/comments"):
			if r.Method == "POST" {
				var v map[string]any
				_ = json.NewDecoder(r.Body).Decode(&v)
				v["performed_via_github_app"] = map[string]int{"id": 1}
				comments = append(comments, v)
				posts++
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{}`))
			} else {
				_ = json.NewEncoder(w).Encode(comments)
			}
		case r.Method == "PATCH":
			state = "closed"
			_, _ = w.Write([]byte(`{}`))
		default:
			n := 1
			if strings.HasSuffix(r.URL.Path, "/2") {
				n = 2
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": state, "comments": len(comments)})
		}
	}))
	defer srv.Close()
	p := testProvider(srv.URL)
	ctx := context.Background()
	if err := p.AddLabels(ctx, "o/r", 1, []string{"bug"}); err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 {
		t.Fatal(labels)
	}
	if err := p.SetLabels(ctx, "o/r", 1, []string{}); err != nil {
		t.Fatal(err)
	}
	if len(labels) != 0 {
		t.Fatal(labels)
	}
	for i := 0; i < 2; i++ {
		if err := p.CloseIssue(ctx, "o/r", 1, forge.CloseIssueSpec{DuplicateOf: 2, Explanation: "Same defect"}); err != nil {
			t.Fatal(err)
		}
	}
	if posts != 1 || state != "closed" {
		t.Fatalf("posts=%d state=%s", posts, state)
	}
	if err := p.CloseIssue(ctx, "o/r", 1, forge.CloseIssueSpec{DuplicateOf: 1, Explanation: "same"}); err == nil {
		t.Fatal("self duplicate")
	}
}
