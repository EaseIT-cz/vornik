package repotest

import (
	"context"
	"sort"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunStepRecallChunksSuite pins MemoryRetrievalAuditRepository.
// StepRecallChunks on both drivers (taint-lineage design §16, GitHub #858):
// every chunk each recall of a task returned, keyed by the recall's execution
// and step, with the chunk's class now.
//
// seedChunk inserts a chunk row. keepsClass is false for a driver whose chunk
// table has no class column (SQLite): a found chunk then reports an empty
// class, which the distiller refuses.
func RunStepRecallChunksSuite(t *testing.T, audits persistence.MemoryRetrievalAuditRepository,
	seedChunk func(t *testing.T, id, projectID, class string), keepsClass bool) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("p858")
	task, other := uniqueID("t858"), uniqueID("t858other")
	exec, step := "exec-1", "research"
	spec, web := uniqueID("chunk-spec"), uniqueID("chunk-web")
	seedChunk(t, spec, project, "spec")
	seedChunk(t, web, project, "external_fetch")
	gone := uniqueID("chunk-gone") // never seeded: deleted since

	rec := func(taskID, execID, stepID string, chunks ...string) {
		t.Helper()
		tid, eid, sid := taskID, execID, stepID
		a := &persistence.MemoryRetrievalAudit{ProjectID: project, TaskID: &tid, Query: "q", ChunkIDs: chunks, RetrievedAt: time.Now().UTC()}
		if execID != "" {
			a.ExecutionID = &eid
		}
		if stepID != "" {
			a.StepID = &sid
		}
		wantOK(t, "record", audits.Record(ctx, a))
	}
	rec(task, exec, step, spec, web)
	rec(task, exec, "empty-recall") // returned nothing
	rec(task, exec, step, gone)     // a chunk deleted since
	rec(task, "", "", spec)         // no execution or step: matches no step
	rec(other, exec, step, web)     // another task's recall is left out

	got, err := audits.StepRecallChunks(ctx, task)
	wantOK(t, "step recall chunks", err)
	type row struct {
		exec, step, chunk, class string
		found                    bool
	}
	var rows []row
	for _, c := range got {
		rows = append(rows, row{c.ExecutionID, c.StepID, c.ChunkID, c.Class, c.Found})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.exec+a.step != b.exec+b.step {
			return a.exec+a.step < b.exec+b.step
		}
		return a.chunk < b.chunk
	})
	class := func(c string) string {
		if keepsClass {
			return c
		}
		return ""
	}
	want := []row{
		{"", "", spec, class("spec"), true},
		{exec, "empty-recall", "", "", false},
		{exec, step, gone, "", false},
		{exec, step, spec, class("spec"), true},
		{exec, step, web, class("external_fetch"), true},
	}
	sort.Slice(want, func(i, j int) bool {
		a, b := want[i], want[j]
		if a.exec+a.step != b.exec+b.step {
			return a.exec+a.step < b.exec+b.step
		}
		return a.chunk < b.chunk
	})
	if len(rows) != len(want) {
		t.Fatalf("StepRecallChunks: %d rows %+v, want %d %+v", len(rows), rows, len(want), want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	none, err := audits.StepRecallChunks(ctx, uniqueID("t858none"))
	wantOK(t, "no recalls", err)
	if len(none) != 0 {
		t.Fatalf("a task with no recalls returned %+v", none)
	}
}
