package api

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// a2aCorpusWalkChunks is the distinct chunks a caller key's A2A tasks may
// retrieve in 24 hours before a2a_corpus_walk names it (#617 slice 2;
// federation design, "Slice 2: the corpus-walk finding"): 2.5 times the
// busiest day measured for all callers together (at least 196 chunks over
// 21 tasks), about 45 tasks at the median of 11 chunks per task.
const a2aCorpusWalkChunks = 500

// SetA2AKeyRetrieval wires a2a_corpus_walk: per caller key, the A2A tasks
// and distinct chunks retrieved since a time. Nil leaves the check SKIPPED.
func (h *DoctorHandlers) SetA2AKeyRetrieval(fn func(ctx context.Context, since time.Time) ([]persistence.A2AKeyRetrieval, error)) {
	h.a2aKeyRetrieval = fn
}

// checkA2ACorpusWalk warns on each caller key whose A2A tasks retrieved more
// than a2aCorpusWalkChunks distinct memory chunks in the last 24 hours. It
// names the key and the count, never a chunk, and states how many keys,
// tasks and chunks it examined, so an OK over no traffic is visibly empty.
// The count can overstate (a widened search writes a row per round, before
// the firewall's pass), never understate.
func (h *DoctorHandlers) checkA2ACorpusWalk(ctx context.Context) DoctorCheck {
	const name = "a2a_corpus_walk"
	if h.a2aKeyRetrieval == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "memory retrieval audit not wired"}
	}
	rows, err := h.a2aKeyRetrieval(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not read the memory retrieval audit: " + err.Error()}
	}
	tasks, chunks := 0, 0
	var over []string
	for _, r := range rows {
		tasks += r.Tasks
		chunks += r.Chunks
		if r.Chunks > a2aCorpusWalkChunks {
			label := r.KeyID
			if r.KeyName != "" {
				label += " (" + r.KeyName + ")"
			}
			over = append(over, fmt.Sprintf("%s: %d distinct chunks over %d A2A task(s)", label, r.Chunks, r.Tasks))
		}
	}
	scope := fmt.Sprintf("%d caller key(s), %d A2A task(s) and %d distinct chunk(s) per key summed, last 24 hours", len(rows), tasks, chunks)
	if len(over) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("%s; no key above %d", scope, a2aCorpusWalkChunks)}
	}
	return DoctorCheck{
		Name:   name,
		Status: "WARNING",
		Message: fmt.Sprintf("%s; %d key(s) above %d distinct chunks. A caller may be walking the expert's corpus: "+
			"review its questions, lower the workflow's a2a.dailyQuotaPerKey, or revoke the key", scope, len(over), a2aCorpusWalkChunks),
		Items: over,
	}
}
