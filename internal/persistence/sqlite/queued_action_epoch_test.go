package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// #119 queued-action gap: a proposal must never adopt a replacement account.
func TestQueuedActionEpoch(t *testing.T) {
	for _, scope := range []string{persistence.AccountNamespaceScope("hermes"), persistence.AccountDaemonScope} {
		t.Run(scope, func(t *testing.T) {
			db := sqlitetest.Memory(t)
			ctx := context.Background()
			now := time.Now().UTC()
			actions := sqlite.NewBrokerActionRepository(db.DB)
			epochs := sqlite.NewAccountTransitionRepository(db.DB)
			a := &persistence.BrokerAction{ProjectID: "hermes--mail", TaskID: "queued", WorkflowID: "reply", ActionKind: "send", Tool: "mcp__mail__send", Status: persistence.BrokerActionStaged, ArgsJSON: []byte(`{}`), ArgsSHA256: "hash", ExpiresAt: now.Add(time.Hour)}
			if _, err := actions.Stage(ctx, a); err != nil {
				t.Fatal(err)
			}
			if _, err := actions.PromoteStaged(ctx, a.TaskID); err != nil {
				t.Fatal(err)
			}
			ticket, err := epochs.Begin(ctx, scope, "owner", "replace", now)
			if err != nil {
				t.Fatal(err)
			}
			if err = epochs.Abort(ctx, ticket); err != nil {
				t.Fatal(err)
			}
			if err = actions.Approve(ctx, a.ActionID, a.ArgsSHA256, "device", now); !errors.Is(err, persistence.ErrAccountTransition) {
				t.Fatalf("stale approval accepted: %v", err)
			}
		})
	}
	t.Run("busy stage", func(t *testing.T) {
		db := sqlitetest.Memory(t)
		ctx := context.Background()
		now := time.Now()
		if _, err := sqlite.NewAccountTransitionRepository(db.DB).Begin(ctx, "ns:hermes", "owner", "replace", now); err != nil {
			t.Fatal(err)
		}
		a := &persistence.BrokerAction{ProjectID: "hermes--mail", TaskID: "busy", ActionKind: "send", Status: persistence.BrokerActionStaged, ArgsJSON: []byte(`{}`), ExpiresAt: now.Add(time.Hour)}
		if _, err := sqlite.NewBrokerActionRepository(db.DB).Stage(ctx, a); !errors.Is(err, persistence.ErrAccountTransition) {
			t.Fatalf("busy stage accepted: %v", err)
		}
	})
}
