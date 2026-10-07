package memory

import (
	"context"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"testing"
	"vornik.io/vornik/internal/memoryfirewall"
)

// Issue #70: only final returned rows reset idle retention, and a failed write
// must not masquerade as a successful last-use reset.
func TestIdleRetention_RecallRecordsOnlyReturnedUse(t *testing.T) {
	for _, api := range []string{"search", "context", "routing"} {
		t.Run(api, func(t *testing.T) {
			r, mock, cleanup := newRepo(t)
			defer cleanup()
			r.SetApprovalRetentionResolver(func(p string) bool { return p == "p" })
			s := NewSearcher(Config{}, r, nil)
			mock.ExpectQuery("ts_rank").WillReturnRows(makeRR([]string{"c1"}, []float64{.5}))
			mock.ExpectQuery("UPDATE project_memory_chunks SET last_used_at").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("c1"))
			var hits []SearchResult
			var err error
			switch api {
			case "search":
				hits, err = s.SearchWithOptions(context.Background(), "p", "q", SearchOptions{Limit: 1})
			case "context":
				hits, err = s.RecallWithContext(context.Background(), "p", "q", SearchOptions{Limit: 1}, memoryfirewall.RequestContext{})
			case "routing":
				hits, _, err = s.RecallWithRouting(context.Background(), "p", "q", SearchOptions{Limit: 1}, memoryfirewall.RequestContext{})
			}
			if err != nil || len(hits) != 1 {
				t.Fatalf("returned use: %v %v", hits, err)
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIdleRetention_FailedRecordAndConcurrentDeletion(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "write-error", true: "delete-won"}[lost], func(t *testing.T) {
			r, mock, cleanup := newRepo(t)
			defer cleanup()
			r.SetApprovalRetentionResolver(func(string) bool { return true })
			s := NewSearcher(Config{}, r, nil)
			mock.ExpectQuery("ts_rank").WillReturnRows(makeRR([]string{"c1"}, []float64{.5}))
			q := mock.ExpectQuery("UPDATE project_memory_chunks SET last_used_at")
			if lost {
				q.WillReturnRows(sqlmock.NewRows([]string{"id"}))
			} else {
				q.WillReturnError(errors.New("usage unavailable"))
			}
			hits, err := s.Search(context.Background(), "p", "q", 1)
			if len(hits) != 0 || (!lost && err == nil) || (lost && err != nil) {
				t.Fatalf("unrecorded hit returned: %v %v", hits, err)
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIdleRetention_FirewallBlockedHitsDoNotResetUse(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	r.SetApprovalRetentionResolver(func(string) bool { return true })
	s := NewSearcher(Config{}, r, nil)
	s.SetFirewall(recentAuditFirewall(memoryfirewall.EnforcementEnforce))
	mock.ExpectQuery("ts_rank").WillReturnRows(makeRR([]string{"clean", "blocked"}, []float64{.5, .4}))
	recentAuditExpectPolicyQuery(mock, "clean", "blocked")
	mock.ExpectQuery("UPDATE project_memory_chunks SET last_used_at").WithArgs("p", pq.Array([]string{"clean"}), pq.Array([]string{"content"})).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("clean"))
	hits, err := s.Search(context.Background(), "p", "q", 2)
	if err != nil || len(hits) != 1 || hits[0].ChunkID != "clean" {
		t.Fatalf("filtered recall: %v %v", hits, err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
