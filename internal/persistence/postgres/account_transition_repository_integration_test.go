//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
	"vornik.io/vornik/internal/persistence"
)

// #119 item 8: rotation between snapshot and terminal update must not
// reactivate a grant. The pre-fence implementation accepted this update.
func TestAccountTransition_ConfirmCannotBypassBusy(t *testing.T) {
	db := accountTransitionFixture(t)
	ctx := context.Background()
	_, err := db.Exec(`INSERT INTO broker_standing_grants(id,project_id,namespace,workflow_id,action,key_paths,key_values,key_hash,max_uses,uses_left,expires_at,created_at,created_by,seed_action_id,reach_hash_at_creation,digest_through,suspended_at) VALUES('fenced','hermes--mail','hermes','reply','send','[]','sealed','hash',5,5,'2027-01-01T00:00:00Z','2026-01-01T00:00:00Z','device','seed','reach','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	epochs := NewAccountTransitionRepository(db.DB)
	if _, err = epochs.Begin(ctx, persistence.AccountNamespaceScope("hermes"), "owner", "rotation", time.Now()); err != nil {
		t.Fatal(err)
	}
	err = NewBrokerGrantRepository(db.DB).Confirm(ctx, "fenced", "old-snapshot")
	if !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("unfenced Confirm during publication = %v; want account transition refusal", err)
	}
}

func TestAccountTransition_StaleSnapshotAndTicket(t *testing.T) {
	db := accountTransitionFixture(t)
	ctx := context.Background()
	r := NewAccountTransitionRepository(db.DB)
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
	db := accountTransitionFixture(t)
	ctx := context.Background()
	r := NewAccountTransitionRepository(db.DB)
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
	if _, err = NewAgentSecretRepository(db.DB).Get(ctx, "hermes", "key"); !errors.Is(err, persistence.ErrNotFound) {
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
	db := accountTransitionFixture(t)
	ctx := context.Background()
	r := NewAccountTransitionRepository(db.DB)
	g := NewBrokerGrantRepository(db.DB)
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

// A pinned connection owns temp shadows, never public/common test tables. The
// repository still owns real short transactions on this connection, so failed
// publication rolls back rather than inheriting an outer fixture transaction.
type accountTransitionTestDB struct{ DB persistence.DBTX }

func TestMigration221_IsAdditive(t *testing.T) {
	db := newIntegrationDB(t)
	tx, err := db.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`CREATE TEMP TABLE agent_secrets (LIKE public.agent_secrets INCLUDING ALL) ON COMMIT DROP`,
		`SET LOCAL search_path=pg_temp`,
		`INSERT INTO agent_secrets(namespace,name,kind,ciphertext,nonce,created_at,updated_at) VALUES('legacy','key','secret',decode('0102','hex'),decode('01','hex'),NOW(),NOW())`,
	} {
		if _, err = tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var absent bool
	if err = tx.QueryRow(`SELECT to_regclass('pg_temp.agent_account_transitions') IS NULL`).Scan(&absent); err != nil || !absent {
		t.Fatal("fixture is not pre-221")
	}
	var up string
	for _, m := range persistence.DefaultMigrations {
		if m.Version == 221 {
			up = m.Up
		}
	}
	if up == "" {
		t.Fatal("migration 221 missing")
	}
	for range 2 {
		if _, err = tx.Exec(up); err != nil {
			t.Fatal(err)
		}
	}
	var preserved bool
	if err = tx.QueryRow(`SELECT ciphertext=decode('0102','hex') FROM agent_secrets WHERE namespace='legacy' AND name='key'`).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("legacy credential changed: %v", err)
	}
	if _, err = tx.Exec(`INSERT INTO agent_account_transitions(scope) VALUES('ns:legacy')`); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	var busy bool
	var owner string
	if err = tx.QueryRow(`SELECT epoch,busy,owner FROM agent_account_transitions WHERE scope='ns:legacy'`).Scan(&epoch, &busy, &owner); err != nil || epoch != 0 || busy || owner != "" {
		t.Fatalf("new transition defaults changed: %v", err)
	}
}

func (d accountTransitionTestDB) Exec(q string, args ...any) (sql.Result, error) {
	return d.DB.ExecContext(context.Background(), q, args...)
}
func accountTransitionFixture(t *testing.T) accountTransitionTestDB {
	t.Helper()
	ctx := context.Background()
	db := newIntegrationDB(t)
	conn, err := db.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tables := []string{"agent_account_transitions", "broker_standing_grants", "agent_secrets", "mcp_oauth_tokens", "agent_integration_approvals"}
	for _, table := range tables {
		if _, err = conn.ExecContext(ctx, "CREATE TEMP TABLE "+table+" (LIKE public."+table+" INCLUDING ALL)"); err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
	}
	if _, err = conn.ExecContext(ctx, "SET search_path=pg_temp"); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range tables {
			if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS pg_temp."+table); err != nil {
				t.Error(err)
			}
		}
		if _, err := conn.ExecContext(ctx, "RESET search_path"); err != nil {
			t.Error(err)
		}
		_ = conn.Close()
	})
	return accountTransitionTestDB{DB: conn}
}

func TestAccountTransition_BeginConfigRejectsStaleSnapshot(t *testing.T) {
	for _, changed := range []string{persistence.AccountNamespaceScope("hermes"), persistence.AccountDaemonScope} {
		t.Run(changed, func(t *testing.T) {
			db := accountTransitionFixture(t)
			ctx := context.Background()
			r := NewAccountTransitionRepository(db.DB)
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
