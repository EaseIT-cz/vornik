package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/subjectstore"
)

// A rights request's stored report keeps every run (GDPR design §4.6.1 for Art
// 17, #900; §4.16.8 for Art 16 and 18, #901): the latest run at the top level,
// plus "run" (1-based) and "earlier_runs", every earlier run's document, oldest
// first. An earlier run can hold what no later run can rebuild: the rows an
// erasure deleted, the inaccurate original a rectification replaced.

// reportWithRuns returns the indented document to store for this run of
// requestID: report at the top level, with "run" spliced in first and
// "earlier_runs" last, so the report keeps its own key order (§7: the narrative
// first). A stored report this run cannot keep refuses the write.
func reportWithRuns(ctx context.Context, repo *subjectstore.Repository, requestID string, report any) ([]byte, error) {
	earlier, err := storedReportRuns(ctx, repo, requestID)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w; the stored report was left untouched and the request is not actioned",
			requestID, err)
	}
	return spliceRuns(report, earlier)
}

// spliceRuns encodes report with "run" first and earlier (oldest first) as
// "earlier_runs" last, indented. report must encode as a JSON object that has
// neither field of its own.
func spliceRuns(report any, earlier []json.RawMessage) ([]byte, error) {
	body, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("a rights report must encode as a JSON object, got %.20s", body)
	}
	if _, ok := fields["run"]; ok {
		return nil, errors.New(`a rights report must not carry its own "run" field`)
	}
	if _, ok := fields["earlier_runs"]; ok {
		return nil, errors.New(`a rights report must not carry its own "earlier_runs" field`)
	}

	var doc bytes.Buffer
	doc.WriteString(`{"run":` + strconv.Itoa(len(earlier)+1))
	if inner := bytes.TrimSpace(body[1 : len(body)-1]); len(inner) > 0 {
		doc.WriteByte(',')
		doc.Write(inner)
	}
	if len(earlier) > 0 {
		runs, err := json.Marshal(earlier)
		if err != nil {
			return nil, err
		}
		doc.WriteString(`,"earlier_runs":`)
		doc.Write(runs)
	}
	doc.WriteByte('}')

	var out bytes.Buffer
	if err := json.Indent(&out, doc.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// storedReportRuns returns the runs already stored on the request, oldest
// first; none before the first run.
func storedReportRuns(ctx context.Context, repo *subjectstore.Repository, requestID string) ([]json.RawMessage, error) {
	stored, _, err := repo.GetRequestReport(ctx, requestID)
	if errors.Is(err, persistence.ErrReportNotRetained) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the stored report: %w", err)
	}
	return earlierReportRuns(stored)
}

// earlierReportRuns unfolds a stored report into its runs, oldest first: the
// runs it carries in earlier_runs, then the stored document itself without
// them. Values stay raw JSON, so nothing an earlier run recorded is re-typed or
// dropped. A report written before #900 (Art 17) or #901 (Art 16/18) has no
// "run" and is run 1. A stored report that is not a JSON object is refused
// rather than overwritten: losing the earlier record is the defect this guards
// against.
func earlierReportRuns(stored string) ([]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stored), &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("the stored report is not a JSON object, so this run cannot keep it")
	}
	var runs []json.RawMessage
	if raw, ok := doc["earlier_runs"]; ok {
		var objects []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &objects); err != nil || json.Unmarshal(raw, &runs) != nil {
			return nil, fmt.Errorf("the stored report's earlier_runs is not a list of run documents")
		}
		for _, o := range objects {
			if o == nil {
				return nil, fmt.Errorf("the stored report's earlier_runs holds a null run")
			}
		}
		delete(doc, "earlier_runs")
	}
	if _, ok := doc["run"]; !ok {
		doc["run"] = json.RawMessage(strconv.Itoa(len(runs) + 1))
	}
	last, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append(runs, last), nil
}
