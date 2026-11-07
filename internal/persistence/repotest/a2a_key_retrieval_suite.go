package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunA2AKeyRetrievalSuite pins MemoryRetrievalAuditRepository.
// A2AChunksByCallerKey on both drivers (#617 slice 2; federation design,
// "Slice 2: the corpus-walk finding"): the distinct chunks the A2A tasks of
// one caller key retrieved since a time, which the a2a_corpus_walk doctor
// check compares with its threshold.
func RunA2AKeyRetrievalSuite(t *testing.T, audits persistence.MemoryRetrievalAuditRepository,
	tasks persistence.TaskRepository, keys persistence.APIKeyRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("p617s2")
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)

	keyA, keyGone, keyOther := uniqueID("key617a"), uniqueID("key617gone"), uniqueID("key617other")
	nameA := uniqueID("caller-a")
	wantOK(t, "create key A", keys.Create(ctx, &persistence.APIKey{ID: keyA, ProjectID: project, Name: nameA,
		KeyHash: uniqueID("hash"), KeyPrefix: "sk-vornik-xx", CreatedAt: now}))

	mkTask := func(src persistence.TaskCreationSource, key string) string {
		t.Helper()
		task := newQueuedTask(project)
		task.CreationSource = src
		if key != "" {
			k := key
			task.CreatedByAPIKeyID = &k
		}
		wantOK(t, "create task", tasks.Create(ctx, task))
		return task.ID
	}
	search := func(taskID string, at time.Time, chunks ...string) {
		t.Helper()
		id := taskID
		wantOK(t, "record", audits.Record(ctx, &persistence.MemoryRetrievalAudit{
			ProjectID: project, TaskID: &id, Query: "q", ChunkIDs: chunks, RetrievedAt: at}))
	}
	recent := now.Add(-time.Hour)

	// Key A: two A2A tasks; c2 and c3 are retrieved twice and count once;
	// c9 was retrieved before the window and is left out.
	a1, a2 := mkTask(persistence.TaskCreationSourceA2A, keyA), mkTask(persistence.TaskCreationSourceA2A, keyA)
	search(a1, recent, "c1", "c2")
	search(a1, recent.Add(time.Second), "c2", "c3")
	search(a2, recent, "c3", "c4")
	search(a2, since.Add(-time.Hour), "c9")
	// A key whose row is gone: counted, with no name.
	search(mkTask(persistence.TaskCreationSourceA2A, keyGone), recent, "c1")
	// Left out: a non-A2A task of a key, and an A2A task with no key.
	search(mkTask(persistence.TaskCreationSourceCompanion, keyOther), recent, "c1", "c5")
	search(mkTask(persistence.TaskCreationSourceA2A, ""), recent, "c6")

	got, err := audits.A2AChunksByCallerKey(ctx, since)
	wantOK(t, "A2AChunksByCallerKey", err)
	byKey := map[string]persistence.A2AKeyRetrieval{}
	for _, r := range got {
		if r.KeyID == "" {
			t.Errorf("a task with no key was counted: %+v", r)
		}
		byKey[r.KeyID] = r
	}
	if r := byKey[keyA]; r != (persistence.A2AKeyRetrieval{KeyID: keyA, KeyName: nameA, Tasks: 2, Chunks: 4}) {
		t.Errorf("key A = %+v, want 2 tasks and 4 distinct chunks", r)
	}
	if r := byKey[keyGone]; r != (persistence.A2AKeyRetrieval{KeyID: keyGone, Tasks: 1, Chunks: 1}) {
		t.Errorf("gone key = %+v, want 1 task, 1 chunk, no name", r)
	}
	if _, ok := byKey[keyOther]; ok {
		t.Errorf("a non-A2A task was counted for %s", keyOther)
	}
	// Most chunks first.
	for i := 1; i < len(got); i++ {
		if got[i].Chunks > got[i-1].Chunks {
			t.Fatalf("not ordered by chunks: %+v", got)
		}
	}
}
