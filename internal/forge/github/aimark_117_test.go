package github

// GitHub #117, Art 50(2) step 2 commit 3, rows 1, 2, 4, 7 (Art 50 design
// §14.8 B): every natural-language body Vornik posts to GitHub ends with a
// hidden signed comment, the mark over the body without that line.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
)

type ghMarkLedger struct {
	mu   sync.Mutex
	rows int
}

func (l *ghMarkLedger) Record(context.Context, persistence.AIOutputMark) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows++
	return nil
}
func (l *ghMarkLedger) RecordKey(context.Context, persistence.AIMarkKey) error { return nil }

var ghMarkRE = regexp.MustCompile(`\n\n<!-- vornik-ai-mark: (\S+) -->$`)

func TestGitHub117_BodiesCarryAHiddenMark(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			_ = json.NewDecoder(r.Body).Decode(&v)
			if b, ok := v["body"].(string); ok {
				mu.Lock()
				bodies = append(bodies, b)
				mu.Unlock()
			}
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"html_url":"https://x/pr/1","number":1}`))
		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet:
			n := 1
			if strings.HasSuffix(r.URL.Path, "/2") {
				n = 2
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "open", "comments": 0})
		default:
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	p := testProvider(srv.URL)
	signer := aimark.NewSigner(make([]byte, 32))
	ledger := &ghMarkLedger{}
	p.SetAIMarker(aimark.NewMarker(signer, ledger, func() string { return "t" }, nil, nil))
	ctx := context.Background()

	if err := p.PostComment(ctx, "o/r", 1, "a comment"); err != nil {
		t.Fatal(err)
	}
	if err := p.PostReview(ctx, "o/r", 1, forge.ReviewSpec{Body: "a review", Event: forge.ReviewComment}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenChangeRequest(ctx, forge.ChangeRequestSpec{Repo: "o/r", Head: "h", Base: "main", Title: "t", Body: "a pr body"}); err != nil {
		t.Fatal(err)
	}
	if err := p.CloseIssue(ctx, "o/r", 1, forge.CloseIssueSpec{DuplicateOf: 2, Explanation: "same defect"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 4 {
		t.Fatalf("bodies posted %d, want 4: %q", len(bodies), bodies)
	}
	for _, b := range bodies {
		mm := ghMarkRE.FindStringSubmatch(b)
		if mm == nil {
			t.Errorf("body without a trailing mark: %q", b)
			continue
		}
		m, err := aimark.Verify(mm[1], signer.Keys())
		if err != nil {
			t.Errorf("mark does not verify: %v", err)
			continue
		}
		sum := sha256.Sum256([]byte(strings.TrimSuffix(b, mm[0])))
		if m.SHA256 != hex.EncodeToString(sum[:]) || m.Surface != "github" {
			t.Errorf("mark %+v is not over the body without its comment: %q", m, b)
		}
	}
	if ledger.rows != 4 {
		t.Errorf("ledger rows %d, want 4", ledger.rows)
	}
}

func TestGitHub117_NoMarkerNoComment(t *testing.T) {
	var posted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]string
		_ = json.NewDecoder(r.Body).Decode(&v)
		posted = v["body"]
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if err := testProvider(srv.URL).PostComment(context.Background(), "o/r", 1, "plain"); err != nil {
		t.Fatal(err)
	}
	if posted != "plain" {
		t.Fatalf("posted %q, want the body unchanged", posted)
	}
}

// A body ending in newlines keeps them: the mark covers the body exactly as
// given, and stripping the mark line returns it (review be88).
func TestGitHub117_TrailingNewlinesSurvive(t *testing.T) {
	var posted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]string
		_ = json.NewDecoder(r.Body).Decode(&v)
		posted = v["body"]
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p := testProvider(srv.URL)
	signer := aimark.NewSigner(make([]byte, 32))
	p.SetAIMarker(aimark.NewMarker(signer, &ghMarkLedger{}, func() string { return "t" }, nil, nil))
	if err := p.PostComment(context.Background(), "o/r", 1, "text\n\n"); err != nil {
		t.Fatal(err)
	}
	mm := ghMarkRE.FindStringSubmatch(posted)
	if mm == nil || strings.TrimSuffix(posted, mm[0]) != "text\n\n" {
		t.Fatalf("posted %q", posted)
	}
	m, _ := aimark.Verify(mm[1], signer.Keys())
	sum := sha256.Sum256([]byte("text\n\n"))
	if m.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("mark %+v", m)
	}
}
