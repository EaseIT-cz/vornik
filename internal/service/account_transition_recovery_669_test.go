package service

// GitHub #669 item 3 (design 22, §22.2.16b): the recovery verb's refusals and
// its effect, over the real SQLite repositories. Found at: production (the
// 2026-10-28 incident: a busy row whose holder was gone, recovered by hand in
// SQL). Regression test at: integration (a real database). Lower than
// e2e-lane because the dead holder is a row a Begin left busy with no process
// behind it, written directly, the state the crash left.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/storage"
)

type openJournal669 struct {
	persistence.ApplyJournalRepository
	rows []*persistence.ConfigApplyJournal
}

func (j *openJournal669) ListOpen(context.Context) ([]*persistence.ConfigApplyJournal, error) {
	return j.rows, nil
}

// recoveryFixture669: a real database, the tracked repository the daemon
// uses, and a raw one that plants a dead holder (no ticket in this process).
func recoveryFixture669(t *testing.T) (accountRecovery, *sqlite.AccountTransitionRepository, *openJournal669) {
	t.Helper()
	ctx := sqlitetest.Context(t)
	backend, err := storage.Open(ctx, config.DatabaseConfig{Driver: "sqlite", Path: filepath.Join(t.TempDir(), "v.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	j := &openJournal669{}
	repos := *backend.Repos
	repos.ApplyJournal = j
	return accountRecovery{c: &Container{repos: &repos}}, sqlite.NewAccountTransitionRepository(backend.DB), j
}

func TestAccountRecovery669_DeadHolderIsRecovered(t *testing.T) {
	ctx := context.Background()
	rec, raw, _ := recoveryFixture669(t)
	tk, err := raw.Begin(ctx, "ns:hermes", "dead", "connection-config", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	rows, examined, err := rec.ListBusy(ctx)
	if err != nil || examined < 1 || len(rows) != 1 || rows[0].Owner != "dead" || rows[0].HeldHere {
		t.Fatalf("list = %+v (examined %d, err %v), want the dead row, not held here", rows, examined, err)
	}
	before, err := rec.Recover(ctx, "ns:hermes", tk.Epoch, "dead")
	if err != nil || before.Owner != "dead" || before.Operation != "connection-config" {
		t.Fatalf("recover: %+v, %v", before, err)
	}
	if _, err := rec.c.repos.AccountTransitions.Begin(ctx, "ns:hermes", "next", "oauth-publication", time.Now().UTC()); err != nil {
		t.Fatalf("a recovered scope cannot Begin again: %v", err)
	}
}

func TestAccountRecovery669_ATicketHeldHereIsRefused(t *testing.T) {
	ctx := context.Background()
	rec, _, _ := recoveryFixture669(t)
	tk, err := rec.c.repos.AccountTransitions.Begin(ctx, "ns:hermes", "live", "connection-config", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rows, _, _ := rec.ListBusy(ctx); len(rows) != 1 || !rows[0].HeldHere {
		t.Fatalf("list does not mark the live ticket held here: %+v", rows)
	}
	if _, err := rec.Recover(ctx, "ns:hermes", tk.Epoch, "live"); !errors.Is(err, api.ErrTransitionHeldHere) {
		t.Fatalf("recovering a held ticket: err %v, want ErrTransitionHeldHere", err)
	}
	// The refused Finish leaves the row busy, so the ticket is still held here.
	if err := rec.c.repos.AccountTransitions.Finish(ctx, persistence.AccountTransitionTicket{Scope: "ns:hermes", Epoch: tk.Epoch + 7, Owner: "live"}, persistence.AccountPublication{}); err == nil {
		t.Fatal("a Finish with a wrong epoch succeeded")
	}
	if _, err := rec.Recover(ctx, "ns:hermes", tk.Epoch, "live"); !errors.Is(err, api.ErrTransitionHeldHere) {
		t.Fatalf("after a failed Finish: err %v, want ErrTransitionHeldHere", err)
	}
}

func TestAccountRecovery669_OpenJournalRowsRefuse(t *testing.T) {
	ctx := context.Background()
	rec, raw, j := recoveryFixture669(t)
	tk, err := raw.Begin(ctx, "ns:hermes", "dead", "connection-config", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// A row for a different project of the same namespace refuses (the
	// any-project rule); one for another namespace does not.
	j.rows = []*persistence.ConfigApplyJournal{{ID: "jr_other", AffectedProjects: []string{"other--mail"}}}
	if _, err := rec.Recover(ctx, "ns:hermes", tk.Epoch, "dead"); err != nil {
		t.Fatalf("another namespace's journal row refused the recovery: %v", err)
	}
	tk2, err := raw.Begin(ctx, "ns:hermes", "dead2", "connection-config", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	j.rows = []*persistence.ConfigApplyJournal{{ID: "jr_same", AffectedProjects: []string{"hermes--calendar"}}}
	var jo *api.JournalOpenError
	if _, err := rec.Recover(ctx, "ns:hermes", tk2.Epoch, "dead2"); !errors.As(err, &jo) || jo.IDs[0] != "jr_same" {
		t.Fatalf("a journal row for another project of the namespace: err %v, want JournalOpenError", err)
	}
	// The daemon scope refuses on any open row.
	dtk, err := raw.Begin(ctx, persistence.AccountDaemonScope, "deadd", "oauth-publication", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	j.rows = []*persistence.ConfigApplyJournal{{ID: "jr_any", AffectedProjects: []string{"plainproject"}}}
	if _, err := rec.Recover(ctx, persistence.AccountDaemonScope, dtk.Epoch, "deadd"); !errors.As(err, &jo) {
		t.Fatalf("the daemon scope with an open row: err %v, want JournalOpenError", err)
	}
}
