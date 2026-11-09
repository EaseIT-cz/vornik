package taintlineage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/taintlineage"
)

// GitHub #670 (task-lifecycle-conversational design §18a): both latch readers
// listed system messages with no limit, so the driver default (100, oldest
// first) applied and a latch written after the 100th system message was
// missed. ReadLatchHashes is the one reader now.
func TestReadLatchHashes_670_LatchAfterTheHundredth(t *testing.T) {
	db := sqlitetest.Memory(t)
	msgs := sqlite.NewTaskMessageRepository(db.DB)
	ctx := context.Background()
	base := time.Now().UTC()
	for i := 0; i < 150; i++ {
		if err := msgs.Insert(ctx, &persistence.TaskMessage{TaskID: "t670", AuthorKind: persistence.TaskMessageAuthorSystem,
			MessageKind: persistence.TaskMessageKindSystem, Content: fmt.Sprintf("system %d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Millisecond)}); err != nil {
			t.Fatal(err)
		}
	}
	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := msgs.Insert(ctx, &persistence.TaskMessage{TaskID: "t670", AuthorKind: persistence.TaskMessageAuthorSystem,
		MessageKind: persistence.TaskMessageKindSystem, Content: "taint latch",
		Metadata: taintlineage.LatchMarkerMetadata(hash), CreatedAt: base.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	got := taintlineage.ReadLatchHashes(ctx, msgs, "t670")
	if len(got) != 1 || got[0] != hash {
		t.Fatalf("ReadLatchHashes = %v, want the one latch written after 150 system messages", got)
	}
}
