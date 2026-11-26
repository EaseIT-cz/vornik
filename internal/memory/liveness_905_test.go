package memory

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// GitHub #905: the URL-liveness checker kept its own private-address list,
// isPrivateLivenessIP, which admitted shapes the one list
// (dialguard.IsBlockedIP) refuses. Memory content could then steer
// `vornikctl memory recheck-urls` into a HEAD request to Alibaba Cloud's
// metadata address, the local host through 0.0.0.0/8, or metadata and
// loopback through NAT64, IPv4-compatible or 6to4 encodings. Design:
// 18-security-and-tenancy.md, "Change (2026-11-24): #905".
var oneListShapes905 = []struct{ name, host string }{
	{"alibaba metadata", "100.100.100.200"},
	{"this-network 0/8", "0.1.2.3"},
	{"limited broadcast", "255.255.255.255"},
	{"nat64 of metadata", "[64:ff9b::a9fe:a9fe]"},
	{"nat64 of loopback", "[64:ff9b::7f00:1]"},
	{"ipv4-compatible of metadata", "[::a9fe:a9fe]"},
	{"6to4 of metadata", "[2002:a9fe:a9fe::1]"},
	{"6to4 of loopback", "[2002:7f00:1::1]"},
}

// recordingDoer answers 200 to every request and records its URL, so a test
// can tell "refused before the request" from "requested and found alive".
type recordingDoer struct {
	mu   sync.Mutex
	seen []string
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.seen = append(d.seen, req.URL.String())
	d.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

// TestRecheckProject905_OneListShapesNeverRequested drives RecheckProject,
// the one call `vornikctl memory recheck-urls` makes, with the production
// address guard (no private-network bypass). Each shape must be refused
// before any request and the chunk flagged dead. Before #905 each was
// requested and, answering 200, confirmed alive.
func TestRecheckProject905_OneListShapesNeverRequested(t *testing.T) {
	for _, tc := range oneListShapes905 {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer func() { _ = db.Close() }()

			u := "http://" + tc.host + "/latest/meta-data/"
			mock.ExpectQuery(regexp.QuoteMeta("SELECT id, content")).
				WithArgs("proj").
				WillReturnRows(sqlmock.NewRows([]string{"id", "content"}).
					AddRow("chunk-1", "see "+u+" for details"))
			mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_chunks SET is_alive")).
				WithArgs(false, sqlmock.AnyArg(), "chunk-1").
				WillReturnResult(sqlmock.NewResult(0, 1))

			doer := &recordingDoer{}
			checker := NewURLLivenessChecker(NewRepository(db))
			checker.SetHTTPClient(doer)

			out, err := checker.RecheckProject(context.Background(), "proj", 0)
			if err != nil {
				t.Fatalf("RecheckProject: %v", err)
			}
			if len(doer.seen) != 0 {
				t.Fatalf("%s was requested (%v); the one list refuses it", tc.host, doer.seen)
			}
			if out.URLsDead != 1 || out.ChunksFlagged != 1 {
				t.Fatalf("want 1 dead URL and 1 flagged chunk, got %+v", out)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
		})
	}
}

// TestLivenessDialContext905_RefusesOneListShapes covers the dial-time check
// (the resolved address, which is what stops DNS rebinding) for the same
// shapes.
func TestLivenessDialContext905_RefusesOneListShapes(t *testing.T) {
	checker := NewURLLivenessChecker(nil)
	for _, tc := range oneListShapes905 {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, err := checker.dialContext(ctx, "tcp", tc.host+":80")
			if conn != nil {
				_ = conn.Close()
			}
			// A refused address errors before any dial; a network error from
			// a real dial attempt would not prove the guard refused it.
			if err == nil || !strings.Contains(err.Error(), "private IP") {
				t.Fatalf("dial to %s was not refused by the guard: %v", tc.host, err)
			}
		})
	}
}
