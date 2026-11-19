package github

// GitHub #244 (github-app-channel design, "Inline review comments and
// installation pacing"). Tests at: integration (the real Provider against
// an httptest GitHub).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/forge"
)

func freshInstallation244(p *Provider, id int64) *Provider {
	p.installationID = id // a gate of its own, untouched by other tests
	return p
}

func TestInline244_CommentsWithCommitAndBodyOnlyFallback(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	reject := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		if _, has := m["comments"]; has && reject {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"pull_request_review_thread.line must be part of the diff"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := freshInstallation244(testProvider(srv.URL), 24401)
	spec := forge.ReviewSpec{Body: "summary", Event: forge.ReviewComment, CommitID: "abc123",
		Comments: []forge.ReviewLineComment{{Path: "a.go", Line: 2, Body: "**blocker**: nil"}}}
	if err := p.PostReview(context.Background(), "o/r", 1, spec); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d posts, want the comments then the body-only fallback", len(bodies))
	}
	first := bodies[0]
	cs, _ := first["comments"].([]any)
	if first["commit_id"] != "abc123" || len(cs) != 1 || cs[0].(map[string]any)["side"] != "RIGHT" || cs[0].(map[string]any)["line"] != float64(2) {
		t.Fatalf("first post = %v", first)
	}
	if _, has := bodies[1]["comments"]; has {
		t.Fatal("the fallback still carried comments")
	}
	// Accepted comments post once.
	reject, bodies = false, nil
	if err := p.PostReview(context.Background(), "o/r", 1, spec); err != nil || len(bodies) != 1 {
		t.Fatalf("accepted comments: %v, %d posts", err, len(bodies))
	}
}

func TestPace244_AHoldRefusesWithoutSendingAcrossProviders(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	limited := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		l := limited
		mu.Unlock()
		if l {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	a := freshInstallation244(testProvider(srv.URL), 24402)
	b := freshInstallation244(testProvider(srv.URL), 24402) // another Provider, same installation
	if err := a.PostComment(context.Background(), "o/r", 1, "x"); err == nil {
		t.Fatal("a rate-limited answer was not an error")
	}
	limited = false
	if err := b.PostComment(context.Background(), "o/r", 1, "y"); err == nil {
		t.Fatal("a call during the hold succeeded")
	}
	if calls != 1 {
		t.Fatalf("the server saw %d calls, want 1: the held call must not be sent", calls)
	}
	time.Sleep(2100 * time.Millisecond)
	if err := b.PostComment(context.Background(), "o/r", 1, "z"); err != nil {
		t.Fatalf("a call after the hold: %v", err)
	}
}

func TestPace244_WritesAreSpaced(t *testing.T) {
	writeSpacing = 300 * time.Millisecond
	t.Cleanup(func() { writeSpacing = 0 })
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	p := freshInstallation244(testProvider(srv.URL), 24403)
	for i := 0; i < 2; i++ {
		if err := p.PostComment(context.Background(), "o/r", 1, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if gap := at[1].Sub(at[0]); gap < 290*time.Millisecond {
		t.Fatalf("two writes %v apart, want at least the spacing", gap)
	}
}
