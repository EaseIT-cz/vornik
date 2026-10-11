package service

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// GitHub #119 item 6 (2026-10-08): the device's memory page lists exactly
// the projects a harness's tools reach plus the broker projects, refuses any
// other, and every forget and export names the device in admin_audit (LLD
// 22, "Memory the user can see", change §3).

type keysFake struct {
	persistence.APIKeyRepository
	rows []*persistence.APIKey
}

func (k keysFake) ListAttributable(context.Context) ([]*persistence.APIKey, error) {
	return k.rows, nil
}

type auditFake struct {
	rows []*persistence.AdminAuditEntry
}

func (a *auditFake) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	a.rows = append(a.rows, e)
	return nil
}
func (a *auditFake) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return a.rows, nil
}

func memoryViewFixture(t *testing.T) (*approverMemoryView, sqlmock.Sqlmock, *auditFake) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := memory.NewRepository(db)
	s := memory.NewSearcher(memory.Config{}, repo, nil)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	revoked := past
	audit := &auditFake{}
	v := &approverMemoryView{d: approverMemoryDeps{
		searcher: func() *memory.Searcher { return s },
		repo:     func() *memory.Repository { return repo },
		keys: keysFake{rows: []*persistence.APIKey{
			{ProjectID: "hermes-memory", ClientKind: "hermes", MemoryRead: true},
			{ProjectID: "companion-example", ClientKind: "claude-code", SessionLabel: "laptop", MemoryRead: true},
			{ProjectID: "no-memory", ClientKind: "codex"},                                          // no memory_read
			{ProjectID: "old-memory", ClientKind: "hermes", MemoryRead: true, RevokedAt: &revoked}, // revoked
			{ProjectID: "gone-memory", ClientKind: "hermes", MemoryRead: true, ExpiresAt: &past},   // expired
			{ProjectID: "hermes--home", ClientKind: "hermes", MemoryRead: true, AgentAdmin: true},  // agent-admin
			{ProjectID: "broker-mail", ClientKind: "hermes", MemoryRead: true},                     // on a broker project
			{ProjectID: "plain", MemoryRead: true},                                                 // no client kind
		}},
		projects: func() []*registry.Project {
			return []*registry.Project{{ID: "broker-mail", Broker: true}, {ID: "hermes--mail", Broker: true}, {ID: "plain"}}
		},
		audit: audit,
		now:   func() time.Time { return now },
	}}
	return v, mock, audit
}

func TestDeviceMemoryView_ListsAssistantAndBrokerProjectsOnly(t *testing.T) {
	v, _, _ := memoryViewFixture(t)
	ps, err := v.Projects(context.Background())
	require.NoError(t, err)
	got := map[string]approverdevice.MemoryProject{}
	for _, p := range ps {
		got[p.ID] = p
	}
	assert.Len(t, got, 4, "%v", ps)
	assert.Equal(t, approverdevice.MemorySectionAssistants, got["hermes-memory"].Section)
	assert.True(t, got["hermes-memory"].Exportable)
	assert.Equal(t, []string{"Claude Code (laptop)"}, got["companion-example"].Holders)
	assert.Equal(t, approverdevice.MemorySectionBroker, got["broker-mail"].Section, "a broker project is section 2 even with a memory key")
	assert.False(t, got["broker-mail"].Exportable)
	assert.Equal(t, "hermes", got["hermes--mail"].Namespace)
	for _, absent := range []string{"no-memory", "old-memory", "gone-memory", "hermes--home", "plain"} {
		_, ok := got[absent]
		assert.False(t, ok, "%s listed", absent)
	}
}

func TestDeviceMemoryView_RefusesAnUnlistedProjectBeforeAnyRead(t *testing.T) {
	v, mock, audit := memoryViewFixture(t)
	ctx := context.Background()
	_, err := v.Page(ctx, "plain", "", 10, "dev_1")
	assert.ErrorIs(t, err, approverdevice.ErrMemoryNotListed)
	_, err = v.Forget(ctx, "plain", "c1", "dev_1")
	assert.ErrorIs(t, err, approverdevice.ErrMemoryNotListed)
	_, err = v.ForgetSource(ctx, "plain", "x", "dev_1")
	assert.ErrorIs(t, err, approverdevice.ErrMemoryNotListed)
	_, err = v.Export(ctx, "broker-mail", "dev_1", func(approverdevice.MemoryExportItem) error { return nil })
	assert.ErrorIs(t, err, approverdevice.ErrMemoryNotListed, "a broker project is not exportable")
	require.NoError(t, mock.ExpectationsWereMet(), "no statement may run for an unlisted project")
	assert.Empty(t, audit.rows)
}

func TestDeviceMemory_ForgetRecordsDeviceRoute(t *testing.T) {
	v, mock, audit := memoryViewFixture(t)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_chunks")).
		WithArgs("hermes-memory", memory.RefuteRouteDeviceForget, "c1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	n, err := v.Forget(context.Background(), "hermes-memory", "c1", "dev_42")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, audit.rows, 1)
	assert.Equal(t, "device:dev_42", audit.rows[0].Principal)
	assert.Equal(t, "device_memory_forget", audit.rows[0].Action)
	assert.Equal(t, "hermes-memory", audit.rows[0].Target)
}

func TestDeviceMemory_ForgetSourceRefutesEveryPageWithTheDeviceRoute(t *testing.T) {
	v, mock, audit := memoryViewFixture(t)
	idRows := sqlmock.NewRows([]string{"id"})
	ids := make([]driver.Value, 0, memory.ListPageMax)
	for i := 0; i < memory.ListPageMax; i++ {
		id := "c" + time.Duration(i).String()
		idRows.AddRow(id)
		ids = append(ids, id)
	}
	mock.ExpectQuery(regexp.QuoteMeta("AND source_name = $2")).WithArgs("hermes-memory", "companion:hermes:note", "", memory.ListPageMax).WillReturnRows(idRows)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_chunks")).
		WithArgs(append([]driver.Value{"hermes-memory", memory.RefuteRouteDeviceForget}, ids...)...).
		WillReturnResult(sqlmock.NewResult(0, int64(memory.ListPageMax)))
	mock.ExpectQuery(regexp.QuoteMeta("AND source_name = $2")).WithArgs("hermes-memory", "companion:hermes:note", ids[len(ids)-1], memory.ListPageMax).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("zz"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_chunks")).
		WithArgs("hermes-memory", memory.RefuteRouteDeviceForget, "zz").
		WillReturnResult(sqlmock.NewResult(0, 1))
	n, err := v.ForgetSource(context.Background(), "hermes-memory", "companion:hermes:note", "dev_42")
	require.NoError(t, err)
	assert.Equal(t, memory.ListPageMax+1, n)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, audit.rows, 1)
	assert.Equal(t, "device_memory_forget_source", audit.rows[0].Action)
	assert.Contains(t, audit.rows[0].After, `"refuted":501`)
}

func TestDeviceMemory_ExportReadsThroughTheFirewallAsAuditReview(t *testing.T) {
	v, mock, audit := memoryViewFixture(t)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("validation_status NOT IN ('refuted','superseded')")).
		WithArgs("hermes-memory", memory.ListPageMax, nil, "").
		WillReturnRows(sqlmock.NewRows([]string{"id", "task_id", "source_name", "content_class", "content", "created_at", "repo_scope", "has_embedding"}).
			AddRow("c1", "", "companion:hermes:note", "decision", "[USER] tea ⟦vm:0123456789abcdef⟧", at, "", true))
	var got []approverdevice.MemoryExportItem
	c, err := v.Export(context.Background(), "hermes-memory", "dev_42", func(it approverdevice.MemoryExportItem) error {
		got = append(got, it)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, approverdevice.MemoryExportCounts{Examined: 1, Exported: 1}, c)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Content, "⟦vm:", "the export is the stored record, verbatim")
	assert.Equal(t, memory.OriginMirrored, got[0].Origin)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, audit.rows, 1)
	assert.Equal(t, "device_memory_export", audit.rows[0].Action)
	assert.Equal(t, "device:dev_42", audit.rows[0].Principal)
	assert.Equal(t, approverMemoryRole, deviceRequest("dev_42").Role)
	assert.EqualValues(t, "audit_review", deviceRequest("dev_42").Purpose)
}
