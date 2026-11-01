package repotest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunAIOutputMarkSuite exercises an AIOutputMarkRepository on whichever
// backend the caller wires (GitHub #117; Art 50 design §14.7). Run from both
// sqlite and postgres: the dialects differ in time and byte types, and
// `go test ./...` is sqlite-only.
func RunAIOutputMarkSuite(t *testing.T, repo persistence.AIOutputMarkRepository) {
	t.Helper()
	ctx := context.Background()
	t1 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	t.Run("first write wins", func(t *testing.T) {
		first := persistence.AIOutputMark{ContentSHA256: "aa11", Surface: "email", Mark: "m1", TaskID: "task-1", IssuedAt: t1}
		if err := repo.Record(ctx, first); err != nil {
			t.Fatal(err)
		}
		again := persistence.AIOutputMark{ContentSHA256: "aa11", Surface: "telegram", Mark: "m2", TaskID: "task-2", IssuedAt: t1.Add(time.Hour)}
		if err := repo.Record(ctx, again); err != nil {
			t.Fatalf("a repeat of the same hash is not an error: %v", err)
		}
		got, err := repo.Get(ctx, "aa11")
		if err != nil {
			t.Fatal(err)
		}
		if got.Mark != "m1" || got.Surface != "email" || got.TaskID != "task-1" || !got.IssuedAt.Equal(t1) {
			t.Fatalf("the first record must stand: %+v", got)
		}
	})

	t.Run("no task id is stored as none", func(t *testing.T) {
		if err := repo.Record(ctx, persistence.AIOutputMark{ContentSHA256: "bb22", Surface: "web", Mark: "m", IssuedAt: t1}); err != nil {
			t.Fatal(err)
		}
		got, err := repo.Get(ctx, "bb22")
		if err != nil || got.TaskID != "" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("unknown hash is not found", func(t *testing.T) {
		if _, err := repo.Get(ctx, "nope"); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("keys: first write wins, oldest first", func(t *testing.T) {
		k1 := persistence.AIMarkKey{KID: "k1", PublicKey: bytes.Repeat([]byte{1}, 32), FirstUsedAt: t1}
		k2 := persistence.AIMarkKey{KID: "k2", PublicKey: bytes.Repeat([]byte{2}, 32), FirstUsedAt: t1.Add(time.Hour)}
		for _, k := range []persistence.AIMarkKey{k2, k1, {KID: "k1", PublicKey: bytes.Repeat([]byte{9}, 32), FirstUsedAt: t1.Add(2 * time.Hour)}} {
			if err := repo.RecordKey(ctx, k); err != nil {
				t.Fatal(err)
			}
		}
		keys, err := repo.Keys(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 2 || keys[0].KID != "k1" || keys[1].KID != "k2" {
			t.Fatalf("keys: %+v", keys)
		}
		if !bytes.Equal(keys[0].PublicKey, k1.PublicKey) || !keys[0].FirstUsedAt.Equal(t1) {
			t.Fatalf("the first k1 must stand: %+v", keys[0])
		}
	})
}
