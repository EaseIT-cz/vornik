package auditredact

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #680 (chat-audit retention and redaction design §12, operator
// decision O-549-1): memory_retrieval_audit stored the recall query as typed,
// so a key pasted into a recall query was kept for the audit window.

type captureRetrievalAudit struct {
	rows []persistence.MemoryRetrievalAudit
}

func (c *captureRetrievalAudit) Record(_ context.Context, a *persistence.MemoryRetrievalAudit) error {
	c.rows = append(c.rows, *a)
	return nil
}
func (c *captureRetrievalAudit) FeedbackStats(context.Context, string, time.Time) (*persistence.MemoryFeedbackStats, error) {
	return &persistence.MemoryFeedbackStats{}, nil
}
func (c *captureRetrievalAudit) UnretrievedChunkIDs(context.Context, string, time.Time, int) ([]string, error) {
	return nil, nil
}
func (c *captureRetrievalAudit) List(context.Context, persistence.MemoryRetrievalAuditFilter) ([]*persistence.MemoryRetrievalAudit, error) {
	return nil, nil
}

func (c *captureRetrievalAudit) A2AChunksByCallerKey(context.Context, time.Time) ([]persistence.A2AKeyRetrieval, error) {
	return nil, nil
}

func TestRetrievalAudit680_RedactsTheQuery(t *testing.T) {
	inner := &captureRetrievalAudit{}
	audit := &recordingAudit{}
	repo := NewRetrievalAudit(inner, detector(t), audit, nil)
	in := &persistence.MemoryRetrievalAudit{ID: "r1", ProjectID: "p1", Query: "why does key " + syntheticKey + " fail", ChunkIDs: []string{"c1"}}
	if err := repo.Record(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(inner.rows) != 1 {
		t.Fatalf("stored %d rows, want 1", len(inner.rows))
	}
	got := inner.rows[0]
	if strings.Contains(got.Query, syntheticKey) || !strings.Contains(got.Query, "[REDACTED") {
		t.Fatalf("stored query %q: the secret must be replaced by its marker", got.Query)
	}
	if got.ID != "r1" || len(got.ChunkIDs) != 1 {
		t.Fatalf("daemon-set fields changed: %+v", got)
	}
	if !strings.Contains(in.Query, syntheticKey) {
		t.Fatal("the caller's struct must not be edited")
	}
	if len(audit.events) == 0 {
		t.Fatal("the redaction must be recorded in the redaction audit")
	}
	// Clean queries are stored unchanged; a nil detector stores unscanned.
	_ = repo.Record(context.Background(), &persistence.MemoryRetrievalAudit{ID: "r2", Query: "vendor contract renewal"})
	if inner.rows[1].Query != "vendor contract renewal" {
		t.Fatalf("a clean query changed: %q", inner.rows[1].Query)
	}
	bypass := NewRetrievalAuditBypassed(inner, ReasonSecretsDisabled, nil)
	_ = bypass.Record(context.Background(), &persistence.MemoryRetrievalAudit{ID: "r3", Query: "k " + syntheticKey})
	if !strings.Contains(inner.rows[2].Query, syntheticKey) {
		t.Fatal("a bypassed seam stores unscanned (and counts it)")
	}
}
