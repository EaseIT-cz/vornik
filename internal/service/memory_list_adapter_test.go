package service

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryfirewall"
)

// GitHub #119 item 6 (2026-10-08): memory_list reads through the memory
// firewall with the key's own request context, and never lists a refuted or
// superseded row (LLD 22, "Memory the user can see").

var memListCols = []string{"id", "task_id", "source_name", "content_class", "content", "created_at", "repo_scope", "has_embedding"}

func memListPolicyRows(allowedRole, blockedRole string) *sqlmock.Rows {
	cols := []string{"id", "tenant_id", "sensitivity_tier", "provenance_source", "provenance_producer", "provenance_trust",
		"provenance_url", "firewall_expires_at", "permitted_roles", "allowed_purposes", "policy_digest", "content_class", "validation_status"}
	return sqlmock.NewRows(cols).
		AddRow("mine", "", "internal", "companion_remember", "", 60, "", nil, pq.Array([]string{allowedRole}), nil, "", "decision", "").
		AddRow("not-mine", "", "internal", "companion_remember", "", 60, "", nil, pq.Array([]string{blockedRole}), nil, "", "decision", "")
}

func TestMemoryList_GoesThroughTheFirewall(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := memory.NewRepository(db)
	s := memory.NewSearcher(memory.Config{}, repo, nil)
	s.SetFirewall(&memory.FirewallDeps{Evaluator: memoryfirewall.NewEvaluator(), EnforcementMode: memoryfirewall.EnforcementEnforce})
	var a api.MemoryCompanionAdapter = &memoryCompanionAdapter{s: s, repo: repo}
	require.NotNil(t, a)
	lister, ok := a.(api.MemoryListAdapter)
	require.True(t, ok, "the companion adapter serves memory_list")

	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("validation_status NOT IN ('refuted','superseded')")).
		WithArgs("alpha", 2, nil, "").
		WillReturnRows(sqlmock.NewRows(memListCols).
			AddRow("mine", "", "companion:hermes:note", "decision", "kept", at, "", true).
			AddRow("not-mine", "", "companion:hermes:note", "decision", "withheld", at.Add(-time.Second), "", true))
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnRows(memListPolicyRows("companion:hermes", "someone-else"))

	page, err := lister.ListMemory(context.Background(), api.MemoryListRequest{
		ProjectID: "alpha", Limit: 2, ActorKind: "companion:hermes", ActorID: "akey_1",
	})
	require.NoError(t, err)
	require.Len(t, page.Entries, 1, "the row the firewall blocks under enforce is dropped")
	assert.Equal(t, "mine", page.Entries[0].ChunkID)
	assert.Equal(t, 2, page.Examined)
	assert.NotEmpty(t, page.NextCursor)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMemoryList_ExcludesRefutedAndSuperseded(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := memory.NewRepository(db)
	var a api.MemoryCompanionAdapter = &memoryCompanionAdapter{s: memory.NewSearcher(memory.Config{}, repo, nil), repo: repo}
	// sqlmock matches by regexp: a statement without the live predicate
	// matches nothing and the call fails.
	mock.ExpectQuery(regexp.QuoteMeta("validation_status NOT IN ('refuted','superseded')")).
		WillReturnRows(sqlmock.NewRows(memListCols))
	_, err = a.(api.MemoryListAdapter).ListMemory(context.Background(), api.MemoryListRequest{ProjectID: "alpha", Limit: 5})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	_, err = a.(api.MemoryListAdapter).ListMemory(context.Background(), api.MemoryListRequest{ProjectID: "alpha", Limit: 5, Cursor: "%%%"})
	assert.ErrorIs(t, err, memory.ErrBadCursor)
}
