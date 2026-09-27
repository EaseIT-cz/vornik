package quality

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"vornik.io/vornik/internal/persistence"
)

// The scorer learned `unscorable` on 2026-09-19 and the durable row did not,
// so every multi-visit execution failed to publish and the reconciler retried
// it every 30 s, forever (agent-quality-benchmark design, amendment
// 2026-09-26; the slow-hardware bench arm logged 197 identical warnings in
// 3 h). These tests guard the CLASS: every ScoreStatus the package declares
// must map to a row the durable layer accepts.

// declaredScoreStatuses reads every `X ScoreStatus = "..."` constant from the
// package source, so a new status cannot be forgotten by a hand-kept list.
func declaredScoreStatuses(t *testing.T) []ScoreStatus {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []ScoreStatus
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "ScoreStatus" {
					continue
				}
				for _, v := range vs.Values {
					if lit, ok := v.(*ast.BasicLit); ok {
						out = append(out, ScoreStatus(strings.Trim(lit.Value, `"`)))
					}
				}
			}
		}
	}
	if len(out) < 5 {
		t.Fatalf("found %d ScoreStatus constants; the AST walk is broken", len(out))
	}
	return out
}

func TestEveryScoreStatusMapsToAValidDurableRow(t *testing.T) {
	for _, status := range declaredScoreStatuses(t) {
		zero := 0.0
		verdict := ExecutionScore{Status: status, Score: &zero}
		if status == ScoreStatusNotApplicable {
			verdict.Score = nil
		}
		row := &persistence.ExecutionQualityScore{ProjectID: "p", TaskID: "t", ExecutionID: "e", WorkflowID: "w",
			Status: string(status), Score: durableScore(verdict)}
		if err := persistence.ValidateExecutionQualityScore(row); err != nil {
			t.Errorf("scorer status %q does not map to a durable row the validator accepts: %v", status, err)
		}
	}
}

func TestExecutionScorePublisher_UnscorablePublishesWithANullScore(t *testing.T) {
	repo := &fakeExecutionScoreRepo{}
	exec := terminalExecution("multi", pinnedPolicy(), multiVisitSnapshot(t, map[string]int{"analyze": 1, "test": 2}))
	if err := NewExecutionScorePublisher(repo, time.Now).Publish(context.Background(), exec); err != nil {
		t.Fatalf("a multi-visit execution must publish: %v", err)
	}
	if len(repo.written) != 1 || repo.written[0].Status != string(ScoreStatusUnscorable) || repo.written[0].Score != nil {
		t.Fatalf("want one unscorable row with a NULL score, got %+v", repo.written)
	}
}

// validatingRepo rejects rows the way the real repositories do.
type validatingRepo struct {
	fakeExecutionScoreRepo
	rejectID string
	upserts  map[string]int
	limits   []int
}

func (v *validatingRepo) Upsert(ctx context.Context, s *persistence.ExecutionQualityScore) error {
	if v.upserts == nil {
		v.upserts = map[string]int{}
	}
	v.upserts[s.ExecutionID]++
	if s.ExecutionID == v.rejectID {
		return errors.Join(persistence.ErrInvalidQualityScore, errors.New("status refused"))
	}
	return v.fakeExecutionScoreRepo.Upsert(ctx, s)
}

func (v *validatingRepo) ListPendingTerminal(_ context.Context, limit int) ([]*persistence.Execution, error) {
	v.limits = append(v.limits, limit)
	if limit > len(v.pending) {
		limit = len(v.pending)
	}
	return v.pending[:limit], nil
}

func TestExecutionScorePublisher_StructuralRejectIsAttemptedOncePerBoot(t *testing.T) {
	repo := &validatingRepo{rejectID: "bad"}
	repo.pending = []*persistence.Execution{terminalExecution("bad", nil, nil), terminalExecution("newer", nil, nil)}
	reg := prometheus.NewRegistry()
	p := NewExecutionScorePublisher(repo, time.Now, NewExecutionScoreMetrics(reg))

	// The rejected row stays pending, as it would in the database, and it
	// is the OLDEST, so a limit of 1 would select only it forever.
	for i := 0; i < 3; i++ {
		_, _ = p.Reconcile(context.Background(), 1)
	}
	if repo.upserts["bad"] != 1 {
		t.Fatalf("a structural reject is permanent: attempted %d times, want 1", repo.upserts["bad"])
	}
	if repo.upserts["newer"] == 0 {
		t.Fatal("a rejected row must not starve a newer execution out of the bounded selection")
	}
	if got := testutil.ToFloat64(p.metrics.RejectedTotal); got != 1 {
		t.Fatalf("rejected counter = %v, want 1", got)
	}

	// A fresh publisher (a restart) tries it again: once per boot.
	p2 := NewExecutionScorePublisher(repo, time.Now)
	_, _ = p2.Reconcile(context.Background(), 1)
	if repo.upserts["bad"] != 2 {
		t.Fatalf("after a restart the rejected execution is attempted again; attempts=%d", repo.upserts["bad"])
	}
}
