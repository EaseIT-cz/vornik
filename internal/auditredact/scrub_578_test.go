package auditredact

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// scrubbingInner is a driver repository that can scrub a stored value.
type scrubbingInner struct {
	conflictRepo
	calls int
}

func (s *scrubbingInner) ReplaceOutputValue(context.Context, string, string, string, string) (int64, error) {
	s.calls++
	return 1, nil
}

// GitHub #578, commit security review 2026-10-31: the executor scrubs a
// carried credential through the repository it holds, which is this
// decorator, so the decorator must pass the scrub to the driver.
func TestReplaceOutputValue578_DelegatesToTheDriver(t *testing.T) {
	inner := &scrubbingInner{}
	var repo persistence.ToolAuditRepository = New(inner, detector(t), nil, &recordingAudit{}, nil, nil)
	sc, ok := repo.(persistence.ToolAuditValueScrubber)
	if !ok {
		t.Fatal("the decorator does not offer ReplaceOutputValue")
	}
	if n, err := sc.ReplaceOutputValue(context.Background(), "e", "t", "v", "m"); err != nil || n != 1 || inner.calls != 1 {
		t.Fatalf("not delegated: n=%d err=%v calls=%d", n, err, inner.calls)
	}
	// An inner store that cannot scrub says so instead of pretending.
	plain := New(&conflictRepo{}, detector(t), nil, &recordingAudit{}, nil, nil)
	if _, err := plain.ReplaceOutputValue(context.Background(), "e", "t", "v", "m"); err == nil {
		t.Fatal("a store that cannot scrub must return an error")
	}
}
