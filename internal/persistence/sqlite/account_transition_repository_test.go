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

// #119 item 8: rotation between snapshot and terminal update must not
// reactivate a grant. The pre-fence implementation accepted this update.
func TestAccountTransition_ConfirmCannotBypassBusy(t *testing.T) {
	db := sqlitetest.Memory(t)
	ctx := context.Background()
	_, err := db.Exec(`INSERT INTO broker_standing_grants(id,project_id,namespace,workflow_id,action,key_paths,key_values,key_hash,max_uses,uses_left,expires_at,created_at,created_by,seed_action_id,reach_hash_at_creation,digest_through,suspended_at) VALUES('fenced','hermes--mail','hermes','reply','send','[]','sealed','hash',5,5,'2027-01-01T00:00:00Z','2026-01-01T00:00:00Z','device','seed','reach','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	epochs := sqlite.NewAccountTransitionRepository(db.DB)
	if _, err = epochs.Begin(ctx, persistence.AccountNamespaceScope("hermes"), "owner", "rotation", time.Now()); err != nil {
		t.Fatal(err)
	}
	err = sqlite.NewBrokerGrantRepository(db.DB).Confirm(ctx, "fenced", "old-snapshot")
	if !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("unfenced Confirm during publication = %v; want account transition refusal", err)
	}
}

func TestAccountTransition_StaleSnapshotAndTicket(t *testing.T) {
	db := sqlitetest.Memory(t)
	ctx := context.Background()
	r := sqlite.NewAccountTransitionRepository(db.DB)
	before, err := r.ReadDecisionEpoch(ctx, "hermes")
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := r.Begin(ctx, persistence.AccountNamespaceScope("hermes"), "first", "rotation", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ReadDecisionEpoch(ctx, "hermes"); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("busy snapshot: %v", err)
	}
	if _, err = r.Begin(ctx, ticket.Scope, "second", "rotation", time.Now()); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("busy Begin: %v", err)
	}
	stale := ticket
	stale.Owner = "other"
	if err = r.Finish(ctx, stale, persistence.AccountPublication{}); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("stale owner: %v", err)
	}
	if err = r.Abort(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	after, err := r.ReadDecisionEpoch(ctx, "hermes")
	if err != nil {
		t.Fatal(err)
	}
	if after[1].Epoch != before[1].Epoch+1 {
		t.Fatalf("epoch rewound: %v -> %v", before, after)
	}
	if err = r.Finish(ctx, ticket, persistence.AccountPublication{}); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("finished ticket reused: %v", err)
	}
}

func TestAccountTransition_FinishAtomicAndDaemonGuard(t *testing.T) {
	db := sqlitetest.Memory(t)
	ctx := context.Background()
	r := sqlite.NewAccountTransitionRepository(db.DB)
	vector, err := r.ReadDecisionEpoch(ctx, "hermes")
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := r.Begin(ctx, persistence.AccountNamespaceScope("hermes"), "owner", "config", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := r.Begin(ctx, persistence.AccountDaemonScope, "daemon-owner", "oauth", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Abort(ctx, daemon); err != nil {
		t.Fatal(err)
	}
	p := persistence.AccountPublication{ExpectedDaemon: &vector[0]}
	if err = r.Finish(ctx, ticket, p); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("changed daemon allowed terminal config: %v", err)
	}
	p.ExpectedDaemon = nil
	p.Secret = &persistence.AgentSecretRow{Namespace: "hermes", Name: "key", Kind: "secret", Ciphertext: []byte("sealed"), Nonce: []byte("nonce"), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	// Invalid identity target must roll back the earlier secret write and busy clear.
	p.Identity = &persistence.AccountIdentityUpdate{ProjectID: "missing", Integration: "missing", Identity: "provider"}
	if err = r.Finish(ctx, ticket, p); err == nil {
		t.Fatal("missing identity target allowed partial publication")
	}
	if _, err = sqlite.NewAgentSecretRepository(db.DB).Get(ctx, "hermes", "key"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("partial secret survived: %v", err)
	}
	if _, err = r.ReadDecisionEpoch(ctx, "hermes"); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("failed Finish cleared busy: %v", err)
	}
	p.Identity = nil
	if err = r.Finish(ctx, ticket, p); err != nil {
		t.Fatal(err)
	}
}

func TestAccountTransition_ConfirmAndReaffirmStaleEpoch(t *testing.T) {
	db := sqlitetest.Memory(t)
	ctx := context.Background()
	r := sqlite.NewAccountTransitionRepository(db.DB)
	g := sqlite.NewBrokerGrantRepository(db.DB)
	_, err := db.Exec(`INSERT INTO broker_standing_grants(id,project_id,namespace,workflow_id,action,key_paths,key_values,key_hash,max_uses,uses_left,expires_at,created_at,created_by,seed_action_id,reach_hash_at_creation,digest_through,suspended_at,account_label,account_identity) VALUES('stale','hermes--mail','hermes','reply','send','[]','sealed','hash',5,5,'2027-01-01T00:00:00Z','2026-01-01T00:00:00Z','device','seed','reach','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z','work','alice')`)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := r.ReadDecisionEpoch(ctx, "hermes")
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := r.Begin(ctx, persistence.AccountNamespaceScope("hermes"), "rotate", "oauth", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Abort(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	if err = g.ConfirmGuarded(ctx, "stale", "reach2", guard); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("stale Confirm: %v", err)
	}
	if err = g.ConfirmAccountGuarded(ctx, "stale", "reach2", "work", "alice", "work", "bob", guard); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("stale Reaffirm: %v", err)
	}
	row, err := g.Get(ctx, "stale")
	if err != nil {
		t.Fatal(err)
	}
	if row.SuspendedAt == nil || row.AccountIdentity != "alice" {
		t.Fatalf("stale terminal mutated row: %+v", row)
	}
	guard, err = r.ReadDecisionEpoch(ctx, "hermes")
	if err != nil {
		t.Fatal(err)
	}
	if err = g.ConfirmAccountGuarded(ctx, "stale", "reach2", "work", "alice", "work", "bob", guard); err != nil {
		t.Fatal(err)
	}
}

func TestAccountTransition_LostTransactionAndRecoveredHolder(t *testing.T) {
	db := sqlitetest.Memory(t)
	ctx := context.Background()
	r := sqlite.NewAccountTransitionRepository(db.DB)
	ticket, err := r.Begin(ctx, persistence.AccountNamespaceScope("hermes"), "holder", "rotation", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	row := &persistence.AgentSecretRow{Namespace: "hermes", Name: "key", Kind: "secret", Ciphertext: []byte("sealed-candidate"), Nonce: []byte("nonce"), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	// A transaction-bound repository stages both row and phase writes. Losing
	// its owner transaction must roll back BOTH, not expose an idle phase.
	if err = sqlite.NewAccountTransitionRepository(tx).Finish(ctx, ticket, persistence.AccountPublication{Secret: row}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = sqlite.NewAgentSecretRepository(db.DB).Get(ctx, "hermes", "key"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("lost transaction exposed secret")
	}
	if _, err = r.ReadDecisionEpoch(ctx, "hermes"); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatal("lost transaction cleared busy")
	}
	if err = sqlite.NewAccountTransitionRepository(tx).Finish(ctx, ticket, persistence.AccountPublication{Secret: row}); err == nil {
		t.Fatal("closed transaction accepted late publication")
	}
	// Drained-holder recovery advances epoch, while a newly constructed
	// repository on the surviving connection refuses the old holder ticket.
	result, err := db.ExecContext(ctx, `UPDATE agent_account_transitions SET epoch=epoch+1,busy=FALSE,owner='',operation='' WHERE scope=? AND epoch=? AND owner=? AND busy=TRUE`, ticket.Scope, ticket.Epoch, ticket.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatal("recovery CAS missed")
	}
	if err = sqlite.NewAccountTransitionRepository(db.DB).Finish(ctx, ticket, persistence.AccountPublication{Secret: row}); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatal("fresh repository accepted old ticket")
	}
	if _, err = sqlite.NewAgentSecretRepository(db.DB).Get(ctx, "hermes", "key"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("old holder published after recovery")
	}
}

func TestAccountTransition_BeginConfigRejectsStaleSnapshot(t *testing.T) {
	for _, changed := range []string{persistence.AccountNamespaceScope("hermes"), persistence.AccountDaemonScope} {
		t.Run(changed, func(t *testing.T) {
			db := sqlitetest.Memory(t)
			ctx := context.Background()
			r := sqlite.NewAccountTransitionRepository(db.DB)
			guard, err := r.ReadDecisionEpoch(ctx, "hermes")
			if err != nil {
				t.Fatal(err)
			}
			rotation, err := r.Begin(ctx, changed, "rotate", "test", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.BeginConfig(ctx, persistence.AccountNamespaceScope("hermes"), "config", "test", time.Now(), guard); !errors.Is(err, persistence.ErrAccountTransition) {
				t.Fatal("busy snapshot accepted config transition")
			}
			if err = r.Abort(ctx, rotation); err != nil {
				t.Fatal(err)
			}
			if _, err = r.BeginConfig(ctx, persistence.AccountNamespaceScope("hermes"), "config", "test", time.Now(), guard); !errors.Is(err, persistence.ErrAccountTransition) {
				t.Fatalf("stale %s snapshot accepted: %v", changed, err)
			}
			fresh, err := r.ReadDecisionEpoch(ctx, "hermes")
			if err != nil {
				t.Fatalf("refused stale begin left busy: %v", err)
			}
			holder, err := r.BeginConfig(ctx, persistence.AccountNamespaceScope("hermes"), "config", "test", time.Now(), fresh)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.Begin(ctx, holder.Scope, "competing", "test", time.Now()); !errors.Is(err, persistence.ErrAccountTransition) {
				t.Fatal("namespace mutation bypassed config busy")
			}
			if err = r.Abort(ctx, holder); err != nil {
				t.Fatal(err)
			}
		})
	}
}
