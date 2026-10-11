package forge

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestIssueValidationAndClipping(t *testing.T) {
	for _, repo := range []string{"a/b", "A/r.git", "one-owner/repo_name"} {
		if err := ValidateIssueRepo(repo); err != nil {
			t.Fatal(repo, err)
		}
	}
	for _, repo := range []string{"", "a/b/c", "a/..", "../b", "a/%2e", "a/" + strings.Repeat("b", 256)} {
		if ValidateIssueRepo(repo) == nil {
			t.Fatal(repo)
		}
	}
	for _, labels := range [][]string{{""}, {"a\n"}, {strings.Repeat("a", 101)}, {string([]byte{255})}, make([]string, 101)} {
		if ValidateIssueLabels(labels) == nil {
			t.Fatal(labels)
		}
	}
	if ValidateIssueLabels([]string{"bug", "priority: high"}) != nil {
		t.Fatal("valid labels")
	}
	f, err := (IssueFilter{}).Resolve()
	if err != nil || f.Page != 1 || f.PerPage != 30 || f.State != "open" {
		t.Fatal(f, err)
	}
	for _, f := range []IssueFilter{{Page: 11}, {Page: -1}, {PerPage: 101}, {PerPage: -1}, {State: "bad"}, {Since: "bad"}, {Labels: []string{""}}} {
		if _, err := f.Resolve(); err == nil {
			t.Fatal(f)
		}
	}
	for _, state := range []string{"closed", "all"} {
		if _, err := (IssueFilter{State: state, Since: "2026-10-11T00:00:00Z"}).Resolve(); err != nil {
			t.Fatal(err)
		}
	}
	if v, trunc := ClipIssueText("short"); v != "short" || trunc {
		t.Fatal(v, trunc)
	}
	if v, trunc := ClipIssueText(strings.Repeat("€", 10000)); !trunc || !utf8.ValidString(v) || len(v) > MaxIssueTextBytes {
		t.Fatal(len(v), trunc)
	}
}
