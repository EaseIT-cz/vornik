package github

// GitHub #838 (github-app-channel design, amendment 2026-11-19): a response
// over the 1 MiB cap is an error, so a diff is never cut silently and then
// reviewed as if it were whole. Found at: production (port of community PR
// EaseIT-cz/vornik#21). Regression test at: integration (an HTTP test server).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchDiff838_OverTheCapIsAnError(t *testing.T) {
	for name, tc := range map[string]struct {
		size    int
		wantErr bool
	}{"exactly the cap": {maxResponseBytes, false}, "one byte over": {maxResponseBytes + 1, true}} {
		body := "diff --git a b\n" + strings.Repeat("x", tc.size-len("diff --git a b\n"))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		diff, err := testProvider(srv.URL).FetchDiff(context.Background(), "o/r", 12)
		srv.Close()
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "size cap") {
				t.Errorf("%s: err = %v, want the size-cap error", name, err)
			}
			continue
		}
		if err != nil || len(diff) != tc.size {
			t.Errorf("%s: %d bytes, %v", name, len(diff), err)
		}
	}
}

// A read that fails mid-body is returned, not dropped (review 82a3 M1).
func TestFetchDiff838_AReadErrorIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("diff --git a b\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				_ = c.Close()
			}
		}
	}))
	defer srv.Close()
	if _, err := testProvider(srv.URL).FetchDiff(context.Background(), "o/r", 12); err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("err = %v, want the read error", err)
	}
}
