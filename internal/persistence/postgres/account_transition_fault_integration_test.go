//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Every fixture pool shares one private schema. No temp table, public test row,
// unrelated backend, or daemon database participates in the fault experiment.
func accountFaultPools(t *testing.T) (*sql.DB, *sql.DB, string) {
	t.Helper()
	admin := newIntegrationDB(t)
	schema := "account_fault_" + strings.ReplaceAll(persistence.GenerateID("s"), "-", "_")
	if _, err := admin.DB.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.DB.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Error(err)
		}
	})
	for _, table := range []string{"agent_account_transitions", "broker_standing_grants", "agent_secrets", "mcp_oauth_tokens", "agent_integration_approvals"} {
		if _, err := admin.DB.Exec("CREATE TABLE " + schema + "." + table + " (LIKE public." + table + " INCLUDING ALL)"); err != nil {
			t.Fatal(err)
		}
	}
	dsn := integrationConfig().DSN() + " search_path=" + schema
	open := func() *sql.DB {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		if err = db.Ping(); err != nil {
			t.Fatal(err)
		}
		return db
	}
	return open(), open(), schema
}
func faultSecret(value string) *persistence.AgentSecretRow {
	now := time.Now().UTC()
	return &persistence.AgentSecretRow{Namespace: "fault", Name: "key", Kind: "secret", Ciphertext: []byte(value), Nonce: []byte("nonce"), CreatedAt: now, UpdatedAt: now}
}
func assertFaultSecret(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	row, err := NewAgentSecretRepository(db).Get(context.Background(), "fault", "key")
	if want == "" {
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("rolled-back secret exists: %v", err)
		}
		return
	}
	if err != nil || string(row.Ciphertext) != want {
		t.Fatalf("committed secret mismatch: %v", err)
	}
}
func seedFaultGrant(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO broker_standing_grants(id,project_id,namespace,workflow_id,action,key_paths,key_values,key_hash,max_uses,uses_left,expires_at,created_at,created_by,seed_action_id,reach_hash_at_creation,digest_through,suspended_at) VALUES('fault','fault--mail','fault','reply','send','[]','sealed','hash',5,5,NOW()+INTERVAL '1 day',NOW(),'device','seed','reach',NOW(),NOW())`)
	if err != nil {
		t.Fatal(err)
	}
}
func TestAccountTransition_SeparatePoolsDecisionSessionLoss(t *testing.T) {
	first, second, _ := accountFaultPools(t)
	ctx := context.Background()
	seedFaultGrant(t, first)
	guard, err := NewAccountTransitionRepository(first).ReadDecisionEpoch(ctx, "fault")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := first.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = checkAccountGuard(ctx, tx, "fault", guard); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err = tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var killed bool
	if err = second.QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&killed); err != nil || !killed {
		t.Fatalf("fixture session termination: %v", err)
	}
	if err = NewBrokerGrantRepository(tx).ConfirmGuarded(ctx, "fault", "stale", guard); err == nil {
		t.Fatal("lost decision transaction accepted terminal update")
	}
	_ = tx.Rollback()
	rotation := NewAccountTransitionRepository(second)
	ticket, err := rotation.Begin(ctx, persistence.AccountNamespaceScope("fault"), "other-pool", "rotation", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = rotation.Finish(ctx, ticket, persistence.AccountPublication{Secret: faultSecret("rotated")}); err != nil {
		t.Fatal(err)
	}
	if err = NewBrokerGrantRepository(first).ConfirmGuarded(ctx, "fault", "stale", guard); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("fresh connection accepted stale decision: %v", err)
	}
	row, err := NewBrokerGrantRepository(second).Get(ctx, "fault")
	if err != nil || row.SuspendedAt == nil {
		t.Fatal("lost/stale decision reactivated grant")
	}
}
func TestAccountTransition_SeparatePoolsFinishSessionLossAndRecovery(t *testing.T) {
	first, second, schema := accountFaultPools(t)
	ctx := context.Background()
	repo := NewAccountTransitionRepository(first)
	ticket, err := repo.Begin(ctx, persistence.AccountNamespaceScope("fault"), "crashed-holder", "rotation", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Kill only this trigger's own fixture session, after the prepared secret
	// write, when Finish is about to clear busy. PostgreSQL rolls back both.
	trigger := fmt.Sprintf(`CREATE FUNCTION %s.kill_finish() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_terminate_backend(pg_backend_pid()); RETURN NEW; END $$; CREATE TRIGGER kill_finish BEFORE UPDATE OF busy ON agent_account_transitions FOR EACH ROW WHEN (OLD.busy AND NOT NEW.busy) EXECUTE FUNCTION %s.kill_finish()`, schema, schema)
	if _, err = second.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	if err = repo.Finish(ctx, ticket, persistence.AccountPublication{Secret: faultSecret("candidate")}); err == nil {
		t.Fatal("killed Finish reported success")
	}
	assertFaultSecret(t, second, "")
	if _, err = NewAccountTransitionRepository(second).ReadDecisionEpoch(ctx, "fault"); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("session loss cleared busy: %v", err)
	}
	if _, err = second.Exec(`DROP TRIGGER kill_finish ON agent_account_transitions`); err != nil {
		t.Fatal(err)
	}
	// Holder call has returned and its connection is dead: recovery is drained.
	tx, err := second.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = NewAgentSecretRepository(tx).Upsert(ctx, *faultSecret("recovered")); err != nil {
		t.Fatal(err)
	}
	res, err := tx.Exec(`UPDATE agent_account_transitions SET epoch=epoch+1,busy=FALSE,owner='',operation='' WHERE scope=$1 AND epoch=$2 AND owner=$3 AND busy`, ticket.Scope, ticket.Epoch, ticket.Owner)
	if err != nil {
		t.Fatal(err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		t.Fatal("controlled recovery CAS missed")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = repo.Finish(ctx, ticket, persistence.AccountPublication{Secret: faultSecret("late-holder")}); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("fresh connection accepted recovered holder ticket: %v", err)
	}
	assertFaultSecret(t, second, "recovered")
}

// commitAckProxy preserves all client bytes, forwarding framed server messages
// after authentication. Arming drops exactly COMMIT CommandComplete BEFORE the
// client sees it; the server has already committed. No SQL/credentials are logged.
type commitAckProxy struct {
	listener    net.Listener
	arm         atomic.Bool
	dropped     chan struct{}
	wg          sync.WaitGroup
	mu          sync.Mutex
	connections []net.Conn
}

func newCommitAckProxy(t *testing.T, target string) *commitAckProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &commitAckProxy{listener: listener, dropped: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			p.mu.Lock()
			p.connections = append(p.connections, client, server)
			p.mu.Unlock()
			p.wg.Add(1)
			go p.forward(client, server)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		p.mu.Lock()
		for _, c := range p.connections {
			_ = c.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return p
}
func (p *commitAckProxy) forward(client, server net.Conn) {
	defer p.wg.Done()
	defer func() { _ = client.Close(); _ = server.Close() }()
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(server, client); _ = server.Close(); close(copied) }()
	defer func() { _ = client.Close(); _ = server.Close(); <-copied }()
	for {
		var header [5]byte
		if _, err := io.ReadFull(server, header[:]); err != nil {
			return
		}
		length := int(binary.BigEndian.Uint32(header[1:]))
		if length < 4 || length > 16<<20 {
			return
		}
		payload := make([]byte, length-4)
		if _, err := io.ReadFull(server, payload); err != nil {
			return
		}
		if header[0] == 'C' && string(payload) == "COMMIT\x00" && p.arm.CompareAndSwap(true, false) {
			close(p.dropped)
			return
		}
		if _, err := client.Write(header[:]); err != nil {
			return
		}
		if _, err := client.Write(payload); err != nil {
			return
		}
	}
}
func TestAccountTransition_CommitAcknowledgementLoss(t *testing.T) {
	_, fresh, schema := accountFaultPools(t)
	config := integrationConfig()
	proxy := newCommitAckProxy(t, net.JoinHostPort(config.Host, fmt.Sprint(config.Port)))
	addr := proxy.listener.Addr().(*net.TCPAddr)
	config.Host = "127.0.0.1"
	config.Port = addr.Port
	db, err := sql.Open("postgres", config.DSN()+" search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	repo := NewAccountTransitionRepository(db)
	ticket, err := repo.Begin(ctx, persistence.AccountNamespaceScope("fault"), "uncertain-holder", "rotation", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	proxy.arm.Store(true)
	if err = repo.Finish(ctx, ticket, persistence.AccountPublication{Secret: faultSecret("committed")}); err == nil {
		t.Fatal("lost COMMIT acknowledgement reported success")
	}
	select {
	case <-proxy.dropped:
	case <-ctx.Done():
		t.Fatal("COMMIT acknowledgement drop was not reached")
	}
	assertFaultSecret(t, fresh, "committed")
	if _, err = NewAccountTransitionRepository(fresh).ReadDecisionEpoch(ctx, "fault"); err != nil {
		t.Fatalf("committed publication did not atomically clear busy: %v", err)
	}
	if err = NewAccountTransitionRepository(fresh).Finish(ctx, ticket, persistence.AccountPublication{Secret: faultSecret("retry")}); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("uncertain ticket retried publication: %v", err)
	}
	assertFaultSecret(t, fresh, "committed")
}
